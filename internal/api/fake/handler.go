package fake

import (
	"context"
	"github.com/oberthci/oberth/internal/apicontract"
	"sync"
)

// Handler records preparation separately from execution and supports blocking
// observations through Run. Tests configure it before concurrent use.
type Handler[I, O any] struct {
	Value        O
	Error        error
	PrepareError error
	Run          func(context.Context) (O, error)
	mu           sync.Mutex
	inputs       []I
	callers      []apicontract.Caller
	calls        int
}

func (h *Handler[I, O]) Prepare(ctx context.Context, c apicontract.Caller, input I) (apicontract.Call[O], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.inputs = append(h.inputs, input)
	h.callers = append(h.callers, c)
	h.mu.Unlock()
	if h.PrepareError != nil {
		return nil, h.PrepareError
	}
	return func(ctx context.Context) (O, error) {
		if err := ctx.Err(); err != nil {
			var zero O
			return zero, err
		}
		h.mu.Lock()
		h.calls++
		h.mu.Unlock()
		if h.Run != nil {
			return h.Run(ctx)
		}
		return h.Value, h.Error
	}, nil
}
func (h *Handler[I, O]) Observed() ([]I, []apicontract.Caller, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]I(nil), h.inputs...), append([]apicontract.Caller(nil), h.callers...), h.calls
}
