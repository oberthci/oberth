package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

type InviteRedeemer interface {
	RedeemInvite(context.Context, json.RawMessage) (any, error)
}

// This MCP endpoint has exactly one capability and never creates an ordinary
// authenticated MCP session. Invite secrets travel only in bounded POST bodies.
func (s *Server) handleInviteMCP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.refuseDuringDrain(w) {
		return
	}
	backend, ok := s.tools.(InviteRedeemer)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if len(r.Header.Values("Authorization")) != 0 || r.Header.Get(mcpSessionHeader) != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "enrollment accepts only an invite request"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 96<<10)
	var incoming rpcRequest
	if err := decodeOne(r.Body, &incoming); err != nil {
		if writeOversizedBody(w, err) {
			return
		}
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error"}})
		return
	}
	response := rpcResponse{JSONRPC: "2.0", ID: normalizeID(incoming.ID)}
	if incoming.JSONRPC != "2.0" || incoming.Method == "" {
		response.Error = &rpcError{Code: -32600, Message: "invalid request"}
		writeRPC(w, response)
		return
	}
	if len(incoming.ID) == 0 {
		if incoming.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		response.Error = &rpcError{Code: -32600, Message: "request ID required"}
		writeRPC(w, response)
		return
	}
	switch incoming.Method {
	case "initialize":
		version, err := negotiateMCPProtocol(incoming.Params)
		if err != nil {
			response.Error = &rpcError{Code: -32602, Message: "invalid protocol version"}
			break
		}
		response.Result = map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "oberth-enrollment", "version": s.version}}
	case "ping":
		response.Result = map[string]any{}
	case "tools/list":
		properties := map[string]any{}
		for _, name := range []string{"secret", "server_url", "ca_pin", "csr", "ssh_public_key"} {
			properties[name] = map[string]any{"type": "string"}
		}
		response.Result = map[string]any{"tools": []map[string]any{{"name": "uplink_redeem", "description": "Redeem a single-use invitation with a local CSR and SSH public key; returns only a client certificate.", "inputSchema": map[string]any{"type": "object", "properties": properties, "required": []string{"secret", "server_url", "ca_pin", "csr", "ssh_public_key"}, "additionalProperties": false}}}}
	case "tools/call":
		var call callParams
		if err := strictUnmarshal(incoming.Params, &call); err != nil || call.Name != "uplink_redeem" {
			response.Error = &rpcError{Code: -32602, Message: "only uplink_redeem is available"}
			break
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		value, err := backend.RedeemInvite(ctx, call.Arguments)
		cancel()
		if err != nil {
			if s.recordAuthFailure(w, r, "invite_refused") {
				return
			}
			// Neither parse errors nor storage failures may echo the invite body.
			response.Result = map[string]any{"isError": true, "content": []map[string]string{{"type": "text", "text": "Invite redemption was refused; check its expiry or ask the admin for a new invitation."}}}
		} else {
			response.Result = toolSuccess(value)
		}
	default:
		response.Error = &rpcError{Code: -32601, Message: "method not found"}
	}
	writeRPC(w, response)
}
