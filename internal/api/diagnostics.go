package api

import "github.com/oberthci/oberth/internal/apicontract"

import "net/http"

func (s *Server) toolTrace(m apicontract.Metadata, h http.Header) *Diagnostic {
	trace := &Diagnostic{Tool: m.Name, Transport: "json", Started: s.config.Runtime.Now(), Outcome: Preparing}
	if m.LongPoll {
		trace.Reference = s.config.Runtime.Nonce()
		if acceptsSSE(h) {
			trace.Transport = "sse"
		}
	}
	return trace
}
func (s *Server) finishTrace(trace *Diagnostic) {
	if trace == nil || trace.Reference == "" {
		return
	}
	trace.Finished = s.config.Runtime.Now()
	if s.config.Diagnostics != nil {
		s.config.Diagnostics.Record(*trace)
	}
}
func firstTrace(traces []*Diagnostic) *Diagnostic {
	if len(traces) == 0 {
		return nil
	}
	return traces[0]
}
func traceOutcome(traces []*Diagnostic, outcome Outcome) {
	if trace := firstTrace(traces); trace != nil {
		trace.Outcome = outcome
	}
}
func (s *Server) failureReference(traces []*Diagnostic) string {
	trace := firstTrace(traces)
	if trace == nil {
		trace = &Diagnostic{Transport: "response", Started: s.config.Runtime.Now(), Outcome: Failed}
		defer s.finishTrace(trace)
	}
	if trace.Reference == "" {
		trace.Reference = s.config.Runtime.Nonce()
	}
	trace.Outcome = Failed
	return trace.Reference
}
