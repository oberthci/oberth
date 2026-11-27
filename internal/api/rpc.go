package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"

	"github.com/oberthci/oberth/internal/apicontract"
)

const rpcResponseBytes = 3*apicontract.ResponseBytes + apicontract.RPCEnvelopeBytes

const RestartingRPCCode = -32001

type rpcRequest = apicontract.MCPRequest
type rpcResponse struct {
	JSONRPC       string          `json:"jsonrpc"`
	ID            json.RawMessage `json:"id"`
	Result        any             `json:"result,omitempty"`
	Error         *rpcError       `json:"error,omitempty"`
	serverVersion string
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}
type callParams struct {
	apicontract.MCPParams
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}
type content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
type toolResult struct {
	Content           []content `json:"content"`
	StructuredContent any       `json:"structuredContent,omitempty"`
	IsError           bool      `json:"isError"`
}

func (s *Server) mcp(w http.ResponseWriter, r *http.Request, c apicontract.Caller) {
	if !jsonMedia(r.Header) || r.URL.RawQuery != "" {
		s.writeError(w, r, refusal("media"), 400)
		return
	}
	raw, err := readBody(w, r, apicontract.RequestBytes)
	if err != nil {
		s.writeError(w, r, err, bodyStatus(err))
		return
	}
	var req rpcRequest
	if err = decode(raw, &req, apicontract.RequestBytes); err != nil {
		s.writeRPC(w, r, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, refusal("input").Reason(), s.publicError(r.Context(), err)}})
		return
	}
	if string(req.Params) == "null" {
		req.Params = nil
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID, serverVersion: s.config.Version}
	if req.JSONRPC != "2.0" || !validRPCID(req.ID) {
		resp.ID = json.RawMessage("null")
		resp.Error = &rpcError{-32600, refusal("input").Reason(), nil}
		s.writeRPC(w, r, resp)
		return
	}
	if !s.validateMCP(w, r, req) {
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(202)
		return
	}
	s.dispatchRPC(w, r, c, rpcExchange{req, resp})
}

func jsonMedia(header http.Header) bool {
	media, _, err := mime.ParseMediaType(header.Get("Content-Type"))
	return err == nil && media == "application/json"
}
func validRPCID(id json.RawMessage) bool {
	if len(id) == 0 {
		return true
	}
	if bytes.Equal(bytes.TrimSpace(id), []byte("null")) {
		return false
	}
	var str string
	if json.Unmarshal(id, &str) == nil {
		return true
	}
	var n json.Number
	return json.Unmarshal(id, &n) == nil
}
func (s *Server) dispatchRPC(w http.ResponseWriter, r *http.Request, c apicontract.Caller, x rpcExchange) {
	req, resp := x.req, x.resp
	switch req.Method {
	case "server/discover", "ping", "tools/list":
		s.discovery(w, r, discoveryRequest{req: req, caller: c})
		return
	case "tools/call":
		s.callTool(w, r, c, rpcExchange{req, resp})
		return
	case "skills/list", "skills/get":
		s.skillRPC(w, r, rpcExchange{req, resp})
		return
	case "resources/list", "resources/read":
		s.resource(w, r, c, rpcExchange{req, resp})
		return
	default:
		resp.Error = &rpcError{-32601, refusal("unknown").Reason(), nil}
	}
	s.writeRPC(w, r, resp)
}
func (s *Server) writeRPC(w http.ResponseWriter, r *http.Request, resp rpcResponse) {
	resp.serverVersion = s.config.Version
	status := 200
	if resp.Error != nil {
		switch resp.Error.Code {
		case -32601:
			status = 404
		case -32700, -32600, -32602, -32020, -32021, -32022:
			status = 400
		}
	}
	s.writeJSON(w, r, jsonReply{status, resp, rpcResponseBytes})
}

type rpcExchange struct {
	req  rpcRequest
	resp rpcResponse
}

const agentInstructions = `Oberth provides explicit CI/CD actions and authoritative run evidence.
Host floor: Claude Code 2.1.331+, interactive terminal main conversation, v2 runtime, direct Anthropic connection and MCP skills enabled; see the agent guide for the complete floor.
Load and approve the installed server skill %s before CI use; a resource read alone does not activate it. Unsupported hosts must upgrade or use the standalone CLI.
Promote only exact passed unassisted evidence.
Resolve uncertain outcomes from durable state before another mutation.
Forms grant no authority.
Keep canonical display blocks whole.`

func (s *Server) discoveryResult(enrollment bool) any {
	capabilities := map[string]any{"tools": map[string]any{}}
	instructions := fmt.Sprintf(agentInstructions, s.skill.entry.URI)
	if enrollment {
		instructions = "Generate and retain private keys locally. Redeem the invitation with uplink_redeem, install the returned certificate, then reconnect to the ordinary tool endpoint."
	} else {
		capabilities["resources"] = map[string]any{}
		capabilities["extensions"] = map[string]any{"io.modelcontextprotocol/skills": map[string]any{}}
	}
	return map[string]any{"supportedVersions": []string{apicontract.ProtocolVersion}, "capabilities": capabilities, "instructions": instructions, "_meta": map[string]any{"io.modelcontextprotocol/serverInfo": map[string]string{"name": "Oberth", "version": s.config.Version}}}
}
