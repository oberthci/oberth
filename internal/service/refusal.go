package service

import "fmt"

// toolRefusal separates a deliberately public reason from its private cause.
// Never put dependency error strings into reason, even when the dependency
// wraps ErrForbidden or ErrUnavailable.
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
