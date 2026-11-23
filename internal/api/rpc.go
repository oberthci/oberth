package api

import (
	"bytes"
	"encoding/json"
	"github.com/oberthci/oberth/internal/apicontract"
	"mime"
	"net/http"
	"time"
)

const rpcResponseBytes = 3*apicontract.ResponseBytes + apicontract.RPCEnvelopeBytes

const RestartingRPCCode = -32001

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}
type callParams struct {
	Name      string                     `json:"name"`
	Arguments json.RawMessage            `json:"arguments,omitempty"`
	Meta      map[string]json.RawMessage `json:"_meta,omitempty"`
}
type initializeParams struct {
	ProtocolVersion string                     `json:"protocolVersion"`
	Capabilities    map[string]json.RawMessage `json:"capabilities,omitempty"`
	ClientInfo      map[string]json.RawMessage `json:"clientInfo,omitempty"`
	Meta            map[string]json.RawMessage `json:"_meta,omitempty"`
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

func validVersion(v string) bool { _, err := time.Parse("2006-01-02", v); return err == nil }
func (s *Server) mcp(w http.ResponseWriter, r *http.Request, c apicontract.Caller) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || r.URL.RawQuery != "" {
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
		s.writeRPC(w, r, rpcResponse{"2.0", json.RawMessage("null"), nil, &rpcError{-32700, refusal("input").Reason(), s.publicError(err)}})
		return
	}
	if string(req.Params) == "null" {
		req.Params = nil
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	if req.JSONRPC != "2.0" || !validRPCID(req.ID) {
		resp.ID = json.RawMessage("null")
		resp.Error = &rpcError{-32600, refusal("input").Reason(), nil}
		s.writeRPC(w, r, resp)
		return
	}
	if req.Method == "notifications/initialized" && len(req.ID) == 0 {
		w.WriteHeader(202)
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(202)
		return
	}
	s.dispatchRPC(w, r, c, rpcExchange{req, resp})
}
func validRPCID(id json.RawMessage) bool {
	if len(id) == 0 || bytes.Equal(id, []byte("null")) {
		return true
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
	case "initialize", "ping", "tools/list":
		s.handshake(w, r, handshakeRequest{req: req, caller: c})
		return
	case "tools/call":
		s.callTool(w, r, c, rpcExchange{req, resp})
		return
	case "resources/list", "resources/read", "resources/subscribe", "resources/unsubscribe":
		s.resource(w, r, c, rpcExchange{req, resp})
		return
	default:
		resp.Error = &rpcError{-32601, refusal("unknown").Reason(), nil}
	}
	s.writeRPC(w, r, resp)
}
func (s *Server) writeRPC(w http.ResponseWriter, r *http.Request, resp rpcResponse) {
	s.writeJSON(w, r, jsonReply{200, resp, rpcResponseBytes})
}

type rpcExchange struct {
	req  rpcRequest
	resp rpcResponse
}

func validProtocolHeader(header http.Header) bool {
	values := header.Values("MCP-Protocol-Version")
	if len(values) == 0 {
		return true
	}
	return len(values) == 1 && (values[0] == "" || values[0] == apicontract.NegotiateProtocol(values[0]))
}

const agentInstructions = `An agent push requests CI with -o oberth.run=1; otherwise it is parked and creates no run.
Keep one background events_wait {kinds:[run.held,run.finished], own:true, after_id, timeout_s:600} open while work is in flight.
Start before pushing, or read the new run once with run_get after starting the wait.
Act on every event, deduplicate IDs, resume with next_after_id even after timed_out; on a gap or lower latest_id read current state and resume from that boundary.
run.finished: read run_get and failure.retry_hint.action. rerun: run_rerun once per code change; if that rerun also asks for rerun, show the reason, then stop and tell the person.
none: read run_logs for failed_step and retry_hint.detail; if the logs show a code fault, fix the code, obtain commit/push consent and push again; otherwise show the reason, detail and log lines to the person and stop.
wait (with after), retry_publication, or an unknown hint: show the reason and hint to the person, then stop; do not take an automatic action.
run.held: read run_hold_list for live availability; inspect with run_hold_exec, push the fix, then release with run_hold_abandon. Never replay an exec after an uncertain reply.
An assisted run is never evidence. Promote only a passed unassisted run of that exact commit.
While promoting, cancel and restart the one wait with promotion.finished added; stop waiting when no work remains.
Whenever builds come up, paste the display block that oberth mcp adds after each run result verbatim in a code block, without clipping reasons.
For issue changes, observe issue.updated and refresh issue_list or issue_get.
object_sha is the accepted object; commit_sha is the tested commit. Confirm delivery with publication_list.
Pushed notifications are an extra. After a lost mutation reply or restart, read durable state before repeating an action.`

func initializeResult(p initializeParams, enrollment bool) any {
	version := apicontract.NegotiateProtocol(p.ProtocolVersion)
	capabilities := map[string]any{"tools": map[string]bool{"listChanged": false}}
	instructions := agentInstructions
	if enrollment {
		instructions = "Generate and retain private keys locally. Redeem the invitation with uplink_redeem, install the returned certificate, then reconnect to the ordinary tool endpoint."
	} else {
		capabilities["resources"] = map[string]bool{"subscribe": true}
	}
	return map[string]any{"protocolVersion": version, "capabilities": capabilities, "serverInfo": map[string]string{"name": "Oberth", "version": "1"}, "instructions": instructions}
}
