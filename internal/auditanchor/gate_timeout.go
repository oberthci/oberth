package auditanchor

import (
	"context"
	"errors"
	"time"

	"github.com/oberthci/oberth/internal/store"
)

// ErrVerificationUnavailable means no permission was granted because verification
// could not finish in time. It does not establish corruption or waive any check.
var ErrVerificationUnavailable = errors.New("audit anchor: verification temporarily unavailable; retry the operation")

// A joined integrity failure takes precedence over a concurrent timeout. Only
// deadline errors (possibly accompanied by known append contention) are transient.
func verificationTimedOut(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) && onlyVerificationTimeouts(err)
}

func onlyVerificationTimeouts(err error) bool {
	// This sentinel deliberately wraps ErrAuditStateChanged. Treat only this
	// exact proven-contention marker as retryable, not arbitrary state changes.
	if err == store.ErrAuditVerificationBusy { //nolint:errorlint // Exact sentinel identity: errors.Is would also accept a joined corruption error.
		return true
	}
	switch cause := err.(type) { //nolint:errorlint // Inspect every unwrap edge; errors.As could skip a non-timeout branch of a joined error.
	case interface{ Unwrap() []error }:
		for _, child := range cause.Unwrap() {
			if !onlyVerificationTimeouts(child) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return onlyVerificationTimeouts(cause.Unwrap())
	default:
		return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, store.ErrAuditVerificationBusy)
	}
}

func (manager *Manager) logGateVerification(state string, attempt int, elapsed time.Duration) {
	if manager.logger != nil {
		manager.logger.Printf("audit_mutation_gate: verification=%s attempt=%d elapsed=%s verify_timeout=%s gate_timeout=%s", state, attempt, elapsed, manager.gateVerifyTimeout, manager.gateTimeout)
	}
}
