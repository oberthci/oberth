package api

import (
	"context"
	"encoding/json"
	"github.com/oberthci/oberth/internal/apicontract"
	"net/http"
)

func (s *Server) callTool(w http.ResponseWriter, r *http.Request, c apicontract.Caller, x rpcExchange) {
	req, resp := x.req, x.resp
	var params callParams
	if err := decode(req.Params, &params, apicontract.RequestBytes); err != nil {
		resp.Error = &rpcError{-32602, refusal("input").Reason(), s.publicError(r.Context(), err)}
		s.writeRPC(w, r, resp)
		return
	}
	if string(params.Arguments) == "null" {
		params.Arguments = nil
	}
	t, ok := s.config.Registry.entries[params.Name]
	if !ok {
		resp.Error = &rpcError{-32602, refusal("unknown").Reason(), nil}
		s.writeRPC(w, r, resp)
		return
	}
	trace := s.toolTrace(t.meta, r.Header)
	defer s.finishTrace(trace)
	release, err := s.reserveCall(c, t.meta)
	if err != nil {
		resp.Result = s.toolFailure(r.Context(), err, trace)
		s.writeRPC(w, r, resp)
		return
	}
	defer release()
	budget := s.rpcBudget(w, r, x.resp)
	ctx := context.WithValue(r.Context(), budgetKey{}, budget)
	if !t.meta.Mutation {
		var cancel context.CancelFunc
		ctx, cancel = s.observationContext(ctx)
		defer cancel()
	}
	p, err := s.config.Registry.prepare(ctx, c, params.Name, params.Arguments)
	if budget.expired {
		return
	}
	if err != nil {
		resp.Result = s.toolFailure(r.Context(), err, trace)
		s.writeRPC(w, r, resp)
		return
	}
	if !p.meta.LongPoll {
		value, err := p.call(ctx)
		if budget.expired {
			return
		}
		resp.Result = s.toolResponse(r.Context(), toolReply{value, err, p.meta.OutputBytes}, trace)
		s.writeRPC(w, r, resp)
		return
	}
	s.longPoll(ctx, w, r, streamInvocation{c, p, resp, trace})
}
func (s *Server) longPoll(ctx context.Context, w http.ResponseWriter, r *http.Request, v streamInvocation) {
	p, resp := v.prepared, v.response
	if acceptsSSE(r.Header) {
		s.streamCall(ctx, w, r, v)
		return
	}

	value, err := p.call(ctx)
	if budget := requestBudget(ctx); budget != nil && budget.expired {
		return
	}
	if s.draining.Load() {
		resp.Error = drainRPC()
	} else {
		resp.Result = s.toolResponse(ctx, toolReply{value, err, p.meta.OutputBytes}, v.trace)
	}
	s.writeRPC(w, r, resp)
}

type toolReply struct {
	value any
	err   error
	limit int
}

func (s *Server) toolResponse(ctx context.Context, reply toolReply, traces ...*Diagnostic) toolResult {
	traceOutcome(traces, Complete)
	if reply.err != nil {
		return s.toolFailure(ctx, reply.err, traces...)
	}
	data, err := encodeBounded(reply.value, reply.limit)
	if err != nil {
		return s.toolFailure(ctx, err, traces...)
	}
	return toolResult{Content: []content{{"text", string(data)}}, StructuredContent: json.RawMessage(data)}
}
func (s *Server) toolFailure(ctx context.Context, err error, traces ...*Diagnostic) toolResult {
	traceOutcome(traces, Failed)
	e := s.publicError(ctx, err, traces...)
	data, _ := json.Marshal(e)
	return toolResult{Content: []content{{"text", string(data)}}, IsError: true}
}
func drainRPC() *rpcError {
	return &rpcError{RestartingRPCCode, refusal("restarting").Reason(), refusal("restarting")}
}

func (s *Server) reserveCall(c apicontract.Caller, m apicontract.Metadata) (func(), error) {
	if !m.LongPoll {
		return func() {}, nil
	}
	release, ok := s.reservations.acquire("wait:"+c.Actor.Identity, pollsPerIdentity)
	if !ok {
		return nil, refusal("longPoll")
	}
	return release, nil
}
