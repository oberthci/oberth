package service

import "fmt"

// toolRefusal separates a deliberately public reason from its private cause.
// Never put dependency error strings into reason, even when the dependency
// wraps a validation, conflict, forbidden or unavailable sentinel.
type toolRefusal struct {
	cause  error
	reason string
}

func (e *toolRefusal) Error() string          { return e.cause.Error() + ": " + e.reason }
func (e *toolRefusal) Unwrap() error          { return e.cause }
func (e *toolRefusal) MCPErrorReason() string { return e.reason }

func refuse(cause error, format string, args ...any) error {
	return &toolRefusal{cause: cause, reason: fmt.Sprintf(format, args...)}
}

// retryOutcomeError carries deliberately public admission information apart
// from the private cause. MCP must retain this even when it redacts the cause.
type retryOutcomeError struct {
	cause   error
	outcome string
}

func (e *retryOutcomeError) Error() string          { return e.outcome + ": " + e.cause.Error() }
func (e *retryOutcomeError) Unwrap() error          { return e.cause }
func (e *retryOutcomeError) MCPWorkOutcome() string { return e.outcome }
