package api

import (
	"encoding/json"
	"net/http"

	"github.com/oberthci/oberth/internal/apicontract"
)

func (s *Server) resource(w http.ResponseWriter, r *http.Request, c apicontract.Caller, x rpcExchange) {
	req, resp := x.req, x.resp
	var params struct {
		apicontract.MCPParams
		URI string `json:"uri,omitempty"`
	}
	if err := decode(req.Params, &params, apicontract.RequestBytes); err != nil {
		resp.Error = &rpcError{-32602, refusal("input").Reason(), nil}
		s.writeRPC(w, r, resp)
		return
	}
	if req.Method == "resources/list" {
		resp.Result = map[string]any{"resources": []any{}}
		s.writeRPC(w, r, resp)
		return
	}
	run, ok := resourceID(params.URI)
	if !ok {
		resp.Error = &rpcError{-32602, refusal("input").Reason(), nil}
		s.writeRPC(w, r, resp)
		return
	}
	arguments, _ := json.Marshal(apicontract.RunGetArgs{ID: run})
	value, err := s.config.Registry.Invoke(r.Context(), c, "run_get", arguments)
	if err != nil {
		e := s.publicError(r.Context(), err)
		resp.Error = &rpcError{-32000, e.Reason(), e}
		s.writeRPC(w, r, resp)
		return
	}
	if req.Method == "resources/read" {
		encoded, _ := json.Marshal(value)
		resp.Result = map[string]any{"contents": []any{map[string]any{"uri": params.URI, "mimeType": "application/json", "text": string(encoded)}}}
	}

	s.writeRPC(w, r, resp)
}
func resourceID(uri string) (string, bool) {
	const prefix = "run://"
	if len(uri) <= len(prefix) || uri[:len(prefix)] != prefix {
		return "", false
	}
	run := uri[len(prefix):]
	for _, r := range run {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return "", false
		}
	}
	return run, true
}
