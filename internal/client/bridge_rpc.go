package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

func (b *bridge) handle(ctx context.Context, req rpcRequest) {
	if req.Method == "tools/call" && len(req.ID) > 0 {
		b.tool(ctx, req)
		return
	}
	if footerRead(req) {
		b.footerRead(ctx, req)
		return
	}
	b.relay(ctx, req)
}

func (b *bridge) relay(ctx context.Context, req rpcRequest) {
	data, err := b.direct(ctx, req)
	if err != nil {
		if len(req.ID) > 0 && ctx.Err() == nil {
			b.rpcError(req.ID, -32000, err)
		}
		if len(req.ID) == 0 {
			b.diagnostic(err)
		}
		return
	}
	if req.Method == "tools/list" {
		data = listedTools(data)
	}
	if len(data) > 0 {
		_ = b.emit(ctx, data)
	}
	if req.Method == "notifications/initialized" && len(req.ID) == 0 {
		b.events.Go(func() { b.followEvents(b.eventCtx) })
	}
}

func (b *bridge) tool(ctx context.Context, req rpcRequest) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      struct {
			ProgressToken json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}
	if json.Unmarshal(req.Params, &params) != nil {
		b.rpcError(req.ID, -32602, failure("bridge_input", nil))
		return
	}
	if len(params.Arguments) == 0 {
		params.Arguments = json.RawMessage(`{}`)
	}
	for _, d := range apicontract.Definitions(apicontract.Bindings{}) {
		if d.Name != params.Name {
			continue
		}
		b.forwardTool(ctx, req, bridgeTool{d, params.Arguments, params.Meta.ProgressToken})
		return
	}
	b.relay(ctx, req)
}

type bridgeTool struct {
	definition       apicontract.Definition
	arguments, token json.RawMessage
}

func (b *bridge) forwardTool(ctx context.Context, req rpcRequest, tool bridgeTool) {
	paths, err := ReserveCredentials(b.config.Base, tool.definition, Options{})
	if err != nil {
		b.rpcError(req.ID, -32000, err)
		return
	}
	defer CleanupCredentials(paths)
	wire := rpcExchange{request: req, headers: b.headers(), progress: b.progress(ctx, tool.token)}
	data, refused, err := b.client.call(ctx, tool.definition, tool.arguments, wire)
	if ctx.Err() != nil {
		if tool.definition.Mutation {
			b.rpcError(req.ID, -32000, failure("bridge_lost", ctx.Err()))
		}
		return
	}
	if err != nil || refused {
		if refused {
			err = resultError(data)
		}
		if tool.definition.Mutation && lostReply(err) {
			err = failure("bridge_lost", err)
		}
		b.rpcError(req.ID, -32000, err)
		return
	}
	data, err = privateToolReply(req.ID, data, paths)
	if err != nil {
		b.rpcError(req.ID, -32000, err)
		return
	}
	_ = b.emit(ctx, b.display(req, tool.definition.Name, data))
}

func privateToolReply(id json.RawMessage, data []byte, paths map[string]string) ([]byte, error) {
	if len(paths) == 0 {
		return data, nil
	}
	payload, refused, _, err := apicontract.ToolPayloadForID(data, id)
	if err != nil || refused {
		return data, err
	}
	public, err := SaveCredentials(payload, paths)
	if err != nil {
		return nil, err
	}
	return editToolReply(data, public, ""), nil
}

func lostReply(err error) bool {
	return retryRequest(err) || errors.Is(err, apicontract.ErrReply) || errors.Is(err, apicontract.ErrReplySize)
}

func (b *bridge) direct(ctx context.Context, req rpcRequest) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout(apicontract.Definition{}, nil))
	defer cancel()
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	resp, err := b.client.send(ctx, http.MethodPost, body, b.headers())
	if err != nil {
		return nil, requestFailure(apicontract.Definition{}, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted && len(req.ID) == 0 {
		return nil, nil
	}
	data, err := apicontract.ReadReply(resp, apicontract.RPCReplyBytes(apicontract.ResponseBytes))
	if err != nil {
		return nil, failure("reply", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resultError(data)
	}
	var reply struct {
		ID      json.RawMessage `json:"id"`
		JSONRPC string          `json:"jsonrpc"`
		Result  struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &reply) != nil || string(reply.ID) != string(req.ID) || reply.JSONRPC != "2.0" {
		return nil, failure("reply", nil)
	}
	if req.Method == "initialize" && len(reply.Error) == 0 {
		b.mu.Lock()
		b.session, b.version = resp.Header.Get("Mcp-Session-Id"), reply.Result.ProtocolVersion
		b.mu.Unlock()
	}
	return data, nil
}

func (b *bridge) rpcError(id json.RawMessage, code int, err error) {
	var public interface {
		Reason() string
		Remedy() string
	}
	if !errors.As(err, &public) {
		err = failure("reply", err)
		_ = errors.As(err, &public)
	}
	detail := map[string]string{"reason": public.Reason(), "remedy": public.Remedy()}
	var known *publicerr.Error
	if errors.As(err, &known) {
		detail["detail"] = known.Detail
	}
	var file interface{ Path() string }
	if errors.As(err, &file) {
		detail["detail"] += "\nFile: " + file.Path()
	}
	data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": public.Reason(), "data": detail}})
	_ = b.emit(b.ctx, data)
}

func (b *bridge) progress(ctx context.Context, token json.RawMessage) func() error {
	if _, ok := rpcID(token); !ok || len(token) == 0 {
		return nil
	}
	count := 0
	return func() error {
		count++
		data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": map[string]any{"progressToken": token, "progress": count}})
		return b.emit(ctx, data)
	}
}

func (b *bridge) display(req rpcRequest, name string, data []byte) []byte {
	if b.config.Display == nil {
		return data
	}
	value, refused, _, err := apicontract.ToolPayloadForID(data, req.ID)
	if err != nil || refused {
		return data
	}
	text, ok := b.config.Display(name, value)
	if !ok {
		return data
	}
	return editToolReply(data, nil, text)
}

func editToolReply(data, payload []byte, text string) []byte {
	var envelope map[string]json.RawMessage
	var result map[string]json.RawMessage
	if json.Unmarshal(data, &envelope) != nil || json.Unmarshal(envelope["result"], &result) != nil {
		return data
	}
	var content []json.RawMessage
	if json.Unmarshal(result["content"], &content) != nil {
		return data
	}
	if payload != nil && len(content) > 0 {
		result["structuredContent"] = payload
		content[0], _ = json.Marshal(map[string]string{"type": "text", "text": string(payload)})
	}
	if text != "" {
		// A host given structured content shows the model only that, so a reply
		// with a display block carries the JSON as its first text block instead.
		// Hosts join text blocks as they are; the leading newline keeps the two apart.
		delete(result, "structuredContent")
		block, _ := json.Marshal(map[string]string{"type": "text", "text": "\n" + text})
		content = append(content, block)
	}
	result["content"], _ = json.Marshal(content)
	envelope["result"], _ = json.Marshal(result)
	encoded, _ := json.Marshal(envelope)
	return encoded
}

// listedTools lists tools without output schemas, because a host requires structured
// content from a tool with one and a reply with a display block has none, and with the
// footer entrypoint on run_list.
func listedTools(data []byte) []byte {
	var envelope, result map[string]json.RawMessage
	var tools []map[string]json.RawMessage
	if json.Unmarshal(data, &envelope) != nil || json.Unmarshal(envelope["result"], &result) != nil || json.Unmarshal(result["tools"], &tools) != nil {
		return data
	}
	for _, tool := range tools {
		delete(tool, "outputSchema")
		withFooter(tool)
	}
	result["tools"], _ = json.Marshal(tools)
	envelope["result"], _ = json.Marshal(result)
	encoded, _ := json.Marshal(envelope)
	return encoded
}

func (b *bridge) diagnostic(err error) {
	if b.config.Diagnostic != nil && b.ctx.Err() == nil {
		var public interface {
			Reason() string
			Remedy() string
		}
		if !errors.As(err, &public) {
			err = failure("connect", err)
			_ = errors.As(err, &public)
		}
		_, _ = fmt.Fprintln(b.config.Diagnostic, public.Reason()+"\n"+public.Remedy())
		var file interface{ Path() string }
		if errors.As(err, &file) {
			_, _ = fmt.Fprintln(b.config.Diagnostic, "File: "+file.Path())
		}
	}
}
