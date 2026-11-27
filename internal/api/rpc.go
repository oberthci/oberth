package api

import (
	"bytes"
	"encoding/json"
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

const agentInstructions = `An agent push requests CI with -o oberth.run=1; otherwise it is parked and creates no run.
Before pushing, call events_wait {snapshot:true, own:true} without after_id; it returns immediately. Save next_after_id, then push.
After pushing, keep one background events_wait {kinds:[run.held,run.finished], own:true, after_id:next_after_id, timeout_s:600} open while work is in flight; if background calls are unavailable, wait only after the push.
Act on every event, deduplicate IDs, resume with next_after_id even after timed_out; on a gap or lower latest_id read current state and resume from that boundary.
run.finished: read run_get; if cancelled, show who cancelled and stop without fixing code or rerunning. Otherwise, when resumed_from is absent, read failure.retry_hint.action. rerun: run_rerun once per code change; if that rerun also asks for rerun, show the reason, then stop and tell the person.
none: read run_logs for failed_step and retry_hint.detail; if the logs show a code fault, fix the code, obtain commit/push consent and push again; otherwise show the reason, detail and log lines to the person and stop.
wait (with after), retry_publication, or an unknown hint: show the reason and hint to the person, then stop; do not take an automatic action.
run.held: read run_hold_list for live availability; inspect with run_hold_exec, push the fix, then release with run_hold_abandon. Never replay an exec after an uncertain reply.
If a long run failed late: fix, commit, after consent push without oberth.run, then run_resume the failed run with that commit.
On run.finished with resumed_from set: if it passed, after consent run_rerun it once; if it failed, fix and resume again.
Never promote a resumed run. An assisted run is never evidence. Promote only a passed unassisted run of that exact commit.
While promoting, cancel and restart the one wait with promotion.finished added; stop waiting when no work remains.
Whenever builds come up, paste the display block that oberth mcp adds after each run result verbatim in a code block, without clipping reasons.
For issue changes, observe issue.updated and refresh issue_list or issue_get.
object_sha is the accepted object; commit_sha is the tested commit. Confirm delivery with publication_list.
Request-scoped progress is an extra. After a lost mutation reply or restart, read durable state before repeating an action.`

func (s *Server) discoveryResult(enrollment bool) any {
	capabilities := map[string]any{"tools": map[string]any{}}
	instructions := agentInstructions
	if enrollment {
		instructions = "Generate and retain private keys locally. Redeem the invitation with uplink_redeem, install the returned certificate, then reconnect to the ordinary tool endpoint."
	} else {
		capabilities["resources"] = map[string]any{}
	}
	return map[string]any{"supportedVersions": []string{apicontract.ProtocolVersion}, "capabilities": capabilities, "instructions": instructions, "_meta": map[string]any{"io.modelcontextprotocol/serverInfo": map[string]string{"name": "Oberth", "version": s.config.Version}}}
}

func jsonMedia(header http.Header) bool {
	media, _, err := mime.ParseMediaType(header.Get("Content-Type"))
	return err == nil && media == "application/json"
}
