package contracttest

import (
	"context"
	"errors"
	"testing"
	"time"
)

// AdmissionCancellation is the producer callback contract. Supply the same
// callback with an event/source fixture; cancellation must end it, never approve.
func AdmissionCancellation(t *testing.T, callback func(context.Context) error, entered <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- callback(ctx) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("admission fixture did not enter cancellable work")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("admission ignored cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("admission callback did not return on cancellation")
	}
}
