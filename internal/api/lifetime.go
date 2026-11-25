package api

import (
	"context"
	"github.com/oberthci/oberth/internal/apicontract"
	"net/http"
	"time"
)

// ownedCall keeps service work joined even if its implementation violates the
// cancellation contract. Only the request owner writes a reply, once at expiry.
func ownedCall[T any](ctx context.Context, run func() (T, error), expiry func()) (T, error) {
	type result struct {
		value T
		err   error
	}
	done := make(chan result, 1)
	go func() { v, err := run(); done <- result{v, err} }()
	select {
	case v := <-done:
		if ctx.Err() == nil {
			return v.value, v.err
		}
		if ctx.Err() == context.DeadlineExceeded {
			expiry()
		}
		var zero T
		return zero, ctx.Err()
	case <-ctx.Done():
		if ctx.Err() == context.DeadlineExceeded {
			expiry()
		}
		<-done
		var zero T
		return zero, ctx.Err()
	}
}

func (s *Server) observationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.observers, cancel)
	return ctx, func() { stop(); cancel() }
}

type budgetKey struct{}
type deliveryBudget struct {
	server     *Server
	writer     http.ResponseWriter
	request    *http.Request
	response   *rpcResponse
	streaming  bool
	expired    bool
	joinWriter func()
}

func requestBudget(ctx context.Context) *deliveryBudget {
	v, _ := ctx.Value(budgetKey{}).(*deliveryBudget)
	return v
}
func (s *Server) rpcBudget(w http.ResponseWriter, r *http.Request, resp rpcResponse) *deliveryBudget {
	return &deliveryBudget{server: s, writer: w, request: r, response: &resp}
}
func (b *deliveryBudget) install(deadline time.Time) {
	_ = http.NewResponseController(b.writer).SetWriteDeadline(deadline.Add(apicontract.WaitMargin))
}
func (b *deliveryBudget) expire(ctx context.Context, m apicontract.Metadata, timeout any) {
	if b.expired {
		return
	}
	b.expired = true
	if b.joinWriter != nil {
		b.joinWriter()
	}
	if b.request.Context().Err() != nil {
		return
	}
	s, w, r := b.server, b.writer, b.request
	if b.response == nil {
		s.writeError(w, r, deadlineError(m), 503)
		_ = http.NewResponseController(w).Flush()
		return
	}
	resp := *b.response
	if s.draining.Load() {
		resp.Error = drainRPC()
	} else if m.LongPoll {
		resp.Result = s.toolResponse(ctx, toolReply{timeout, nil, m.OutputBytes})
	} else {
		resp.Result = s.toolFailure(ctx, deadlineError(m))
	}
	if b.streaming {
		data, err := encodeBounded(resp, rpcResponseBytes)
		if err == nil {
			_ = s.frame(w, "event: message\ndata: "+string(data)+"\n\n")
		}
	} else {
		s.writeRPC(w, r, resp)
		_ = http.NewResponseController(w).Flush()
	}
}
func deadlineError(m apicontract.Metadata) error {
	if !m.Mutation {
		return messages()["deadline"].caused(context.DeadlineExceeded)
	}
	e := messages()["actionTimeout"].caused(context.DeadlineExceeded)
	e.Detail = "Read tool: " + m.Check
	return e
}
func (s *Server) httpBudget(w http.ResponseWriter, r *http.Request) context.Context {
	b := &deliveryBudget{server: s, writer: w, request: r}
	return context.WithValue(r.Context(), budgetKey{}, b)
}
