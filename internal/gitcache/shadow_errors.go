package gitcache

import (
	"context"
	"errors"
	"io"
)

type shadowReason uint8

const (
	shadowCLIFailed shadowReason = iota
	shadowHashGuard
	shadowGoOther
	shadowOversize
	shadowCanceled
	shadowAttrGap
	shadowReasonCount
)

var shadowReasonNames = [...]string{"cli_failed", "gogit_hash_guard", "gogit_other", "oversize", "canceled", "expected_attr_gap"}

// Fixed helper exit codes are a protocol, not an interpretation of stderr.
const (
	shadowExitCLI       = 20
	shadowExitHashGuard = 21
	shadowExitGoOther   = 22
	shadowExitOversize  = 23
	shadowExitCanceled  = 24
	shadowExitAttrGap   = 25
)

var errShadowOversize = errors.New("oversized shadow output")

type shadowFailure struct {
	reason shadowReason
	cause  error
}

func (e *shadowFailure) Error() string { return shadowReasonNames[e.reason] }
func (e *shadowFailure) Unwrap() error { return e.cause }

func classifyShadowError(err error) shadowReason {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return shadowCanceled
	case errors.Is(err, errGoObjectIntegrity):
		return shadowHashGuard
	case errors.Is(err, errShadowOversize):
		return shadowOversize
	case errors.Is(err, errArchiveAttrGap):
		return shadowAttrGap
	}
	var failure *shadowFailure
	if errors.As(err, &failure) {
		return failure.reason
	}
	return shadowGoOther
}
func shadowStageError(ctx context.Context, reason shadowReason, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return &shadowFailure{reason, err}
}
func shadowExitCode(err error) int {
	if err == nil {
		return 0
	}
	switch classifyShadowError(err) {
	case shadowCLIFailed:
		return shadowExitCLI
	case shadowHashGuard:
		return shadowExitHashGuard
	case shadowOversize:
		return shadowExitOversize
	case shadowCanceled:
		return shadowExitCanceled
	case shadowAttrGap:
		return shadowExitAttrGap
	default:
		return shadowExitGoOther
	}
}
func shadowExitReason(code int) shadowReason {
	switch code {
	case shadowExitCLI:
		return shadowCLIFailed
	case shadowExitHashGuard:
		return shadowHashGuard
	case shadowExitOversize:
		return shadowOversize
	case shadowExitCanceled:
		return shadowCanceled
	case shadowExitAttrGap:
		return shadowAttrGap
	default:
		return shadowGoOther
	}
}

// RunShadowHelper is the process-only entrypoint. Never call this in the server:
// OOM preference belongs to the expendable observer, not production delivery.
func RunShadowHelper(ctx context.Context, args []string, output io.Writer) int {
	if len(args) != 4 {
		return shadowExitGoOther
	}
	if err := prepareShadowHelper(); err != nil {
		return shadowExitGoOther
	}
	return shadowExitCode(RunShadow(ctx, args[0], args[1], args[2], args[3], output))
}
