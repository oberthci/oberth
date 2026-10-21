package restartretry

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"
	"time"
)

func TestTransientClassification(t *testing.T) {
	for _, err := range []error{io.EOF, io.ErrUnexpectedEOF, syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EPIPE} {
		if !Transient(fmt.Errorf("wrapped: %w", err)) {
			t.Fatalf("not transient: %v", err)
		}
	}
	dialTimeout := &net.OpError{Op: "dial", Net: "tcp", Err: timeoutError{}}
	if !Transient(fmt.Errorf("Post: %w", dialTimeout)) {
		t.Fatal("dial timeout during a rollout is not transient")
	}
	readTimeout := &net.OpError{Op: "read", Net: "tcp", Err: timeoutError{}}
	dialFailure := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("no route")}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, x509.UnknownAuthorityError{}, errors.New("bad JSON"), readTimeout, dialFailure} {
		if Transient(err) {
			t.Fatalf("retried permanent error: %v", err)
		}
	}
}

type timeoutError struct{}

func (timeoutError) Error() string { return "i/o timeout" }
func (timeoutError) Timeout() bool { return true }

func TestBudgetWindow(t *testing.T) {
	var budget Budget
	if !budget.Wait(t.Context()) {
		t.Fatal("first reconnect refused")
	}
	// A Recreate rollout drains for up to 75 seconds before the replacement starts.
	if window := time.Until(budget.until); window < 115*time.Second || window > DefaultWindow {
		t.Fatalf("default window = %s", window)
	}
	budget = Budget{Window: 50 * time.Millisecond}
	deadline := time.Now().Add(time.Second)
	for budget.Wait(t.Context()) {
		if time.Now().After(deadline) {
			t.Fatal("configured window ignored")
		}
	}
}

func TestBudgetCancellationAndExpiry(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var budget Budget
	if budget.Wait(ctx) {
		t.Fatal("retried canceled request")
	}
	budget.until = time.Now().Add(-time.Second)
	if budget.Wait(t.Context()) {
		t.Fatal("retried expired outage budget")
	}
	ctx, cancel = context.WithTimeout(t.Context(), 5*time.Millisecond)
	defer cancel()
	budget = Budget{}
	if budget.Wait(ctx) {
		t.Fatal("ignored original deadline")
	}
}
