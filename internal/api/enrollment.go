package api

import (
	"context"
	"net/http"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
)

func (s *Server) enrollment(w http.ResponseWriter, r *http.Request) {
	if s.draining.Load() {
		s.writeError(w, r, refusal("restarting"), 503)
		return
	}
	if !s.enrollmentAllowed(r) {
		s.writeError(w, r, refusal("unknown"), 404)
		return
	}
	if !jsonMedia(r.Header) {
		s.writeError(w, r, refusal("media"), 400)
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
	if !s.validateMCP(w, r, req) {
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(202)
		return
	}
	if s.discovery(w, r, discoveryRequest{req: req, enrollment: true}) {
		return
	}
	s.enrollmentTool(w, r, req)
}

func (s *Server) enrollmentTool(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	var params callParams
	if !enrollmentRequest(req, &params) {
		resp.Error = &rpcError{-32601, refusal("unknown").Reason(), nil}
		s.writeRPC(w, r, resp)
		return
	}
	var args model.InviteRedemption
	if err := decode(params.Arguments, &args, apicontract.EnrollmentBytes); err != nil {
		resp.Result = s.toolFailure(r.Context(), refusal("input"))
		s.writeRPC(w, r, resp)
		return
	}
	if !validRedemption(args) {
		resp.Result = s.toolFailure(r.Context(), refusal("input"))
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
	resp.Result = s.toolResponse(r.Context(), toolReply{value, nil, apicontract.ResponseBytes})
	s.writeRPC(w, r, resp)
}
func (s *Server) enrollmentFailure(w http.ResponseWriter, r *http.Request, resp rpcResponse, err error) {
	if err != nil && s.config.Classify(err).Code() != publicerr.Forbidden {
		resp.Result = s.toolFailure(r.Context(), err)
		s.writeRPC(w, r, resp)
		return
	}
	resp.Result = s.toolFailure(r.Context(), refusal("invitation"))
	s.writeRPC(w, r, resp)
}

func enrollmentRequest(req rpcRequest, params *callParams) bool {
	if req.JSONRPC != "2.0" || req.Method != "tools/call" || !validRPCID(req.ID) {
		return false
	}
	return decode(req.Params, params, apicontract.EnrollmentBytes) == nil && params.Name == "uplink_redeem"
}

func (s *Server) enrollmentAllowed(r *http.Request) bool {
	return s.config.Enroll != nil && r.URL.RawQuery == "" && len(r.Header.Values("Authorization")) == 0
}
func validRedemption(a model.InviteRedemption) bool {
	return a.Secret != "" && a.CSR != "" && a.CAPin != "" && a.ServerURL != "" && a.SSHPublicKey != ""
}
