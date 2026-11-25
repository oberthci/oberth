package api

import (
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"net/http"
	"time"
)

type handshakeRequest struct {
	req        rpcRequest
	caller     apicontract.Caller
	enrollment bool
}

func (s *Server) handshake(w http.ResponseWriter, r *http.Request, x handshakeRequest) bool {
	resp := rpcResponse{JSONRPC: "2.0", ID: x.req.ID}
	limit := apicontract.RequestBytes
	if x.enrollment {
		limit = apicontract.EnrollmentBytes
	}
	switch x.req.Method {
	case "initialize":
		resp = s.initializeReply(w, r, x, limit)
	case "ping":
		var p apicontract.Empty
		if err := decode(x.req.Params, &p, limit); err != nil {
			resp.Error = &rpcError{-32602, refusal("input").Reason(), s.publicError(r.Context(), err)}
		} else {
			resp.Result = apicontract.Empty{}
		}
	case "tools/list":
		var p struct {
			Cursor string `json:"cursor,omitempty"`
		}
		if err := decode(x.req.Params, &p, limit); err != nil {
			resp.Error = &rpcError{-32602, refusal("input").Reason(), s.publicError(r.Context(), err)}
		} else if x.enrollment {
			resp.Result = enrollmentCatalogue()
		} else {
			resp.Result = s.config.Registry.listWire(x.caller)
		}
	case "notifications/initialized":
		w.WriteHeader(202)
		return true
	default:
		return false
	}
	s.writeRPC(w, r, resp)
	return true
}
func (s *Server) initializeReply(w http.ResponseWriter, r *http.Request, x handshakeRequest, limit int) rpcResponse {
	resp := rpcResponse{JSONRPC: "2.0", ID: x.req.ID}
	var p initializeParams
	if decode(x.req.Params, &p, limit) != nil || !validVersion(p.ProtocolVersion) {
		resp.Error = &rpcError{-32602, refusal("method").Reason(), nil}
		return resp
	}
	if !x.enrollment {
		id, err := s.sessions.create(x.caller, s.config.Runtime)
		if err != nil {
			problem := s.publicError(r.Context(), err)
			resp.Error = &rpcError{-32000, problem.Reason(), problem}
			return resp
		}
		w.Header().Set("Mcp-Session-Id", id)
	}
	resp.Result = initializeResult(p, x.enrollment)
	return resp
}
func enrollmentCatalogue() any {
	schema := schemaForType[model.InviteRedemption]()
	for name, property := range schema["properties"].(apicontract.Schema) {
		property.(apicontract.Schema)["description"] = redemptionDescription(name)
	}
	schema["required"] = []string{"secret", "server_url", "ca_pin", "csr", "ssh_public_key"}
	return struct {
		Tools []apicontract.Metadata `json:"tools"`
	}{[]apicontract.Metadata{{Name: "uplink_redeem", Description: "Redeem an invitation with locally generated public keys. Install the returned certificate and reconnect to the ordinary endpoint.", InputSchema: schema, OutputSchema: schemaForType[model.InviteCertificate](), InputBytes: apicontract.EnrollmentBytes, OutputBytes: apicontract.ResponseBytes, Deadline: 15 * time.Second}}}
}
