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

type timeout struct{}

func (timeout) Error() string { return "timeout" }
func (timeout) Timeout() bool { return true }
func TestTransientWholeTree(t *testing.T) {
	for _, err := range []error{io.EOF, io.ErrUnexpectedEOF, syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EPIPE, &net.OpError{Op: "dial", Err: timeout{}}} {
		if !Transient(fmt.Errorf("wrapped: %w", err)) || !Transient(errors.Join(err, io.EOF)) {
			t.Fatalf("refused %v", err)
		}
	}
	for _, err := range []error{nil, context.Canceled, context.DeadlineExceeded, x509.UnknownAuthorityError{}, errors.New("protocol"), &net.OpError{Op: "read", Err: timeout{}}} {
		if Transient(err) || Transient(errors.Join(io.EOF, err, errors.New("permanent"))) {
			t.Fatalf("retried %v", err)
		}
	}
	if Transient(fmt.Errorf("wrapper: %w", errors.Join(io.EOF, context.Canceled))) {
		t.Fatal("mixed join retried")
	}
}
func TestHandoverStatuses(t *testing.T) {
	for _, status := range []int{502, 503, 504} {
		if !RetryableStatus(status) {
			t.Fatal(status)
		}
	}
	for _, status := range []int{0, 200, 401, 403, 429, 500} {
		if RetryableStatus(status) {
			t.Fatal(status)
		}
	}
}

func TestBudgetBackoffOriginalDeadlineAndZero(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	budget := New(start, time.Time{})
	now := start
	for _, want := range []time.Duration{100, 200, 400, 800, 1600, 2000, 2000} {
		delay, ok := budget.Next(now)
		if !ok || delay != want*time.Millisecond {
			t.Fatalf("delay %v %v", delay, ok)
		}
		now = now.Add(delay)
	}
	if _, ok := budget.Next(start.Add(defaultWindow)); ok {
		t.Fatal("expired budget renewed")
	}
	budget = New(start, start.Add(100*time.Millisecond))
	if _, ok := budget.Next(start); ok {
		t.Fatal("retry at deadline")
	}
	budget = New(start, start.Add(101*time.Millisecond))
	if _, ok := budget.Next(start); !ok {
		t.Fatal("earlier retry refused")
	}
	for _, budget := range []Budget{{}, New(time.Time{}, time.Time{}), New(start, start.Add(-time.Second))} {
		if _, ok := budget.Next(start); ok {
			t.Fatal("zero or expired budget retried")
		}
	}
}
func TestWaitCancellationAndImmediateCompletion(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if !errors.Is(Wait(ctx, time.Hour), context.Canceled) {
		t.Fatal("cancellation lost")
	}
	if err := Wait(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	active, cancelActive := context.WithCancel(t.Context())
	finished := make(chan error, 1)
	go func() { finished <- Wait(active, time.Hour) }()
	cancelActive()
	if !errors.Is(<-finished, context.Canceled) {
		t.Fatal("wait did not join on cancellation")
	}
}

func TestDialTimeoutDoesNotHideJoinedPermanentLeaf(t *testing.T) {
	mixed := &net.OpError{Op: "dial", Err: errors.Join(timeout{}, errors.New("permission refused"))}
	if Transient(fmt.Errorf("wrapped: %w", mixed)) {
		t.Fatal("dial wrapper hid permanent leaf")
	}
	if !Transient(&net.OpError{Op: "dial", Err: errors.Join(timeout{}, io.EOF)}) {
		t.Fatal("pure reconnect join refused")
	}
}

func TestWaitTimerCompletion(t *testing.T) {
	if err := Wait(t.Context(), time.Nanosecond); err != nil {
		t.Fatal(err)
	}
}

func TestStreamBackoffUntilSessionCancellation(t *testing.T) {
	b := Stream()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, want := range []time.Duration{2, 4, 8, 16, 30, 30} {
		delay, ok := b.Next(now)
		if !ok || delay != want*time.Second {
			t.Fatal("stream backoff changed", delay)
		}
		now = now.Add(time.Hour)
	}
}
