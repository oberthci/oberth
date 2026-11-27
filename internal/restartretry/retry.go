package restartretry

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"
)

const defaultWindow = 2 * time.Minute

// Budget is a single outage window, starting at the first failure. Construct
// with New at that failure. Its zero value refuses retries. It is caller-owned.
type Budget struct {
	until   time.Time
	delay   time.Duration
	ceiling time.Duration
}

// New bounds retries by the earlier of the outage window and original deadline.
// A zero deadline means no caller deadline; a zero start refuses all retries.
func New(start, deadline time.Time) Budget {
	if start.IsZero() {
		return Budget{}
	}
	until := start.Add(defaultWindow)
	if !deadline.IsZero() && deadline.Before(until) {
		until = deadline
	}
	return Budget{until: until, delay: 100 * time.Millisecond, ceiling: 2 * time.Second}
}

// Next advances backoff to the constructor's ceiling. It never schedules a
// retry at or beyond an expiry and never renews the original deadline.
func (b *Budget) Next(now time.Time) (time.Duration, bool) {
	if now.IsZero() || b.delay <= 0 || !b.until.IsZero() && !now.Add(b.delay).Before(b.until) {
		return 0, false
	}
	delay := b.delay
	b.delay = min(2*b.delay, b.ceiling)
	return delay, true
}

// Wait blocks for a supplied delay or cancellation. The caller owns the retry
// decision and budget; cancellation must also be checked before resending.
func Wait(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

// RetryableStatus recognizes only public service handover responses.
func RetryableStatus(code int) bool {
	return code == http.StatusServiceUnavailable || code == http.StatusBadGateway || code == http.StatusGatewayTimeout
}

// Transient requires every joined branch to be a reconnectable failure.
// Request cancellation, response-read timeouts, trust and protocol errors refuse.
func Transient(err error) bool { return transient(err, false) }

func transient(err error, dial bool) bool {
	if err == nil {
		return false
	}
	if _, ok := err.(interface{ Timeout() bool }); ok {
		var operation *net.OpError
		if errors.As(err, &operation) && operation != nil {
			dial = operation.Op == "dial"
		}
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		return allTransient(joined.Unwrap(), dial)
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		if child := wrapped.Unwrap(); child != nil {
			return transient(child, dial)
		}
	}
	if reconnectLeaf(err) {
		return true
	}
	timeout, ok := err.(interface{ Timeout() bool })
	return dial && ok && timeout.Timeout()
}
func allTransient(children []error, dial bool) bool {
	if len(children) == 0 {
		return false
	}
	for _, child := range children {
		if !transient(child, dial) {
			return false
		}
	}
	return true
}

func reconnectLeaf(err error) bool {
	for _, kind := range []error{io.EOF, io.ErrUnexpectedEOF, syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EPIPE} {
		if errors.Is(err, kind) {
			return true
		}
	}
	return false
}
