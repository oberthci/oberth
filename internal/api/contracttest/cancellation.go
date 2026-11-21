package contracttest

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/apicontract"

	"testing"
	"testing/synctest"
)

// BlockingHandler exposes synchronization at the implementation's blocking I/O.
// Release is an emergency test cleanup; successful cancellation must not need it.
type BlockingHandler[I, O any] struct {
	Handler          apicontract.Handler[I, O]
	Started, Stopped <-chan struct{}
	Release          func()
}
type CancellationCase[I, O any] struct {
	New    func(*testing.T) BlockingHandler[I, O]
	Caller apicontract.Caller
	Input  I
}

// HandlerCancellation proves cancellation during execution and joining of owned
// work. C2 supplies a real operation blocked at an injected dependency.
func HandlerCancellation[I, O any](t *testing.T, c CancellationCase[I, O]) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		fixture := c.New(t)
		defer fixture.Release()
		call, err := fixture.Handler.Prepare(t.Context(), c.Caller, c.Input)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { _, err := call(ctx); done <- err }()
		<-fixture.Started
		cancel()
		synctest.Wait()
		select {
		case err = <-done:
		default:
			fixture.Release()
			<-done
			t.Fatal("execution did not join on cancellation")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatal("cancelled execution lost its cause", err)
		}
		select {
		case <-fixture.Stopped:
		default:
			t.Fatal("execution returned with work still running")
		}
	})
}
