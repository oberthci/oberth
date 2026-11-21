package api

import (
	"context"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"net/http"
	"time"
)

func (s *Server) enrollment(w http.ResponseWriter, r *http.Request) {
	if s.draining.Load() {
		s.writeError(w, r, refusal("restarting"), 503)
		return
	}
	if !validProtocolHeader(r.Header) {
		s.writeError(w, r, refusal("method"), 400)
		return
	}
	if !s.enrollmentAllowed(r) {
		s.writeError(w, r, refusal("unknown"), 404)
		return
	}
	raw, err := readBody(w, r, apicontract.EnrollmentBytes)
	if err != nil {
		s.writeError(w, r, err, bodyStatus(err))
		return
	}
	var req rpcRequest
	if err := decode(raw, &req, apicontract.EnrollmentBytes); err != nil {
		s.writeError(w, r, refusal("input"), 400)
		return
	}
	if req.JSONRPC != "2.0" || !validRPCID(req.ID) {
		s.writeError(w, r, refusal("input"), 400)
		return
	}
	if s.handshake(w, r, handshakeRequest{req: req, enrollment: true}) {
		return
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	var params callParams
	if !enrollmentRequest(req, &params) {
		resp.Error = &rpcError{-32601, refusal("unknown").Reason(), nil}
		s.writeRPC(w, r, resp)
		return
	}
	var args model.InviteRedemption
	if err := decode(params.Arguments, &args, apicontract.EnrollmentBytes); err != nil {
		resp.Result = s.toolFailure(refusal("input"))
		s.writeRPC(w, r, resp)
		return
	}
	if !validRedemption(args) {
		resp.Result = s.toolFailure(refusal("input"))
		s.writeRPC(w, r, resp)
		return
	}
	s.redeem(w, r, args, resp)
}
func (s *Server) redeem(w http.ResponseWriter, r *http.Request, args model.InviteRedemption, resp rpcResponse) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	call, err := s.config.Enroll.Prepare(ctx, apicontract.Caller{}, args)
	if err != nil {
		s.enrollmentFailure(w, r, resp, err)
		return
	}
	if call == nil {
		s.enrollmentFailure(w, r, resp, err)
		return
	}
	value, err := call(ctx)
	if err != nil {
		s.enrollmentFailure(w, r, resp, err)
		return
	}
	resp.Result = s.toolResponse(value, nil, apicontract.ResponseBytes)
	s.writeRPC(w, r, resp)
}
func (s *Server) enrollmentFailure(w http.ResponseWriter, r *http.Request, resp rpcResponse, err error) {
	if err != nil && s.config.Classify(err).Code() != publicerr.Forbidden {
		resp.Result = s.toolFailure(err)
		s.writeRPC(w, r, resp)
		return
	}
	resp.Result = s.toolFailure(refusal("invitation"))
	s.writeRPC(w, r, resp)
}

func enrollmentRequest(req rpcRequest, params *callParams) bool {
	if req.JSONRPC != "2.0" || req.Method != "tools/call" || !validRPCID(req.ID) {
		return false
	}
	return decode(req.Params, params, apicontract.EnrollmentBytes) == nil && params.Name == "uplink_redeem"
}

func (s *Server) enrollmentAllowed(r *http.Request) bool {
	return s.config.Enroll != nil && r.URL.RawQuery == "" && len(r.Header.Values("Authorization")) == 0 && r.Header.Get("Mcp-Session-Id") == ""
}
func validRedemption(a model.InviteRedemption) bool {
	return a.Secret != "" && a.CSR != "" && a.CAPin != "" && a.ServerURL != "" && a.SSHPublicKey != ""
}
