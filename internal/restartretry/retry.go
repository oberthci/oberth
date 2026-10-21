// Package restartretry bounds reconnects for idempotent observations across a
// short server outage. Callers must never use it for mutating requests.
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

// DefaultWindow covers a Recreate rollout: the old pod's graceful drain, the
// replacement's image pull and startup, and readiness after adoption.
const DefaultWindow = 120 * time.Second

// Budget starts at the first transient failure, including for long-lived SSE
// reads. The caller's original deadline always takes precedence. A zero
// Window uses DefaultWindow.
type Budget struct {
	Window time.Duration
	until  time.Time
	delay  time.Duration
}

func Transient(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		dialTimeout(err)
}

// A connect that times out never reached the server, so nothing was sent:
// during a rollout the Service can blackhole SYNs until an endpoint is ready.
func dialTimeout(err error) bool {
	var operation *net.OpError
	return errors.As(err, &operation) && operation.Op == "dial" && operation.Timeout()
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
		window := budget.Window
		if window <= 0 {
			window = DefaultWindow
		}
		budget.until = now.Add(window)
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
