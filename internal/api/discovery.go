package api

import (
	"net/http"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
)

type discoveryRequest struct {
	req        rpcRequest
	caller     apicontract.Caller
	enrollment bool
}

func (s *Server) discovery(w http.ResponseWriter, r *http.Request, x discoveryRequest) bool {
	resp := rpcResponse{JSONRPC: "2.0", ID: x.req.ID}
	limit := apicontract.RequestBytes
	if x.enrollment {
		limit = apicontract.EnrollmentBytes
	}
	switch x.req.Method {
	case "server/discover":
		resp.Result = s.discoveryResult(x.enrollment)
	case "ping":
		var p apicontract.MCPParams
		if err := decode(x.req.Params, &p, limit); err != nil {
			resp.Error = &rpcError{-32602, refusal("input").Reason(), s.publicError(r.Context(), err)}
		} else {
			resp.Result = apicontract.Empty{}
		}
	case "tools/list":
		var p struct {
			apicontract.MCPParams
			Cursor string `json:"cursor,omitempty"`
		}
		if err := decode(x.req.Params, &p, limit); err != nil {
			resp.Error = &rpcError{-32602, refusal("input").Reason(), s.publicError(r.Context(), err)}
		} else if x.enrollment {
			resp.Result = enrollmentCatalogue()
		} else {
			resp.Result = s.config.Registry.listWire(x.caller)
		}
	default:
		return false
	}
	s.writeRPC(w, r, resp)
	return true
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
