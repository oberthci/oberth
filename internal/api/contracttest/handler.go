package contracttest

import (
	"context"
	"github.com/oberthci/oberth/internal/apicontract"
	"reflect"
	"testing"
)

// HandlerCase supplies an authorized fixture for a concrete service operation.
type HandlerCase[I, O any] struct {
	New    func(*testing.T) apicontract.Handler[I, O]
	Caller apicontract.Caller
	Input  I
	Want   O
}

// Handler runs against the fake and each service implementation. Preparation
// must respect cancellation and execution must preserve the complete typed value.
func Handler[I, O any](t *testing.T, c HandlerCase[I, O]) {
	t.Helper()
	t.Run("typed result", func(t *testing.T) {
		h := c.New(t)
		call, err := h.Prepare(t.Context(), c.Caller, c.Input)
		if err != nil {
			t.Fatal(err)
		}
		got, err := call(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, c.Want) {
			t.Fatalf("result differs: %#v", got)
		}
	})
	t.Run("cancelled preparation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := c.New(t).Prepare(ctx, c.Caller, c.Input)
		if err == nil {
			t.Fatal("cancelled preparation succeeded")
		}
	})
	t.Run("cancelled execution", func(t *testing.T) {
		call, err := c.New(t).Prepare(t.Context(), c.Caller, c.Input)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := call(ctx); err == nil {
			t.Fatal("cancelled execution succeeded")
		}
	})
}
