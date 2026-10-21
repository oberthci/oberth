package restartretry

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
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
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, x509.UnknownAuthorityError{}, errors.New("bad JSON")} {
		if Transient(err) {
			t.Fatalf("retried permanent error: %v", err)
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
