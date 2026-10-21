// Package restartretry bounds reconnects for idempotent observations across a
// short server outage. Callers must never use it for mutating requests.
package restartretry

import (
	"context"
	"errors"
	"io"
	"net/http"
	"syscall"
	"time"
)

// Budget starts at the first transient failure, including for long-lived SSE
// reads. The caller's original deadline always takes precedence.
type Budget struct {
	until time.Time
	delay time.Duration
}

func Transient(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

func Status(code int) bool {
	return code == http.StatusServiceUnavailable || code == http.StatusBadGateway || code == http.StatusGatewayTimeout
}

func (budget *Budget) Wait(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	now := time.Now()
	if budget.until.IsZero() {
		budget.until = now.Add(30 * time.Second)
	}
	if budget.delay == 0 {
		budget.delay = 100 * time.Millisecond
	}
	delay := min(budget.delay, time.Until(budget.until))
	if delay <= 0 {
		return false
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		budget.delay = min(2*budget.delay, 2*time.Second)
		return time.Now().Before(budget.until)
	}
}
