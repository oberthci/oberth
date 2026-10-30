package argojob

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/oberthci/oberth/internal/model"
)

const profileReadTimeout = 5 * time.Second
const profileRetryMinimum = 250 * time.Millisecond
const profileRetryMaximum = 10 * time.Second
const profileLogInterval = 30 * time.Second

// Keep the underlying error for classification, never for serialization: API
// errors can include response bodies unrelated to the public controller proof.
type profileReadError struct {
	message, object string
	cause           error
}

func (e *profileReadError) Error() string { return e.message }
func (e *profileReadError) Unwrap() error { return e.cause }

func profileReadClass(err error) string {
	switch {
	case strings.Contains(err.Error(), "rate limiter"), strings.Contains(err.Error(), "rate: Wait"), apierrors.IsTooManyRequests(err):
		return "throttled"
	case errors.Is(err, context.DeadlineExceeded), apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		return "timeout"
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return "access_denied"
	default:
		return "api_read"
	}
}

func (controller *Controller) ControllerProfileStatus() *model.ControllerProfileStatus {
	if controller.config.NonrootProfile == "" {
		return nil
	}
	controller.profileMu.Lock()
	defer controller.profileMu.Unlock()
	status := controller.profileStatus
	if status.State == "" {
		status.State = "unverified"
	}
	status.AdmissionPaused = status.State != "verified"
	if status.AdmissionPaused {
		status.Remedy = "Restore one Ready controller matching the pinned profile and its immutable ConfigMap; verify ReplicaSet readiness and server read permissions. Obsolete Failed/Evicted records are accepted automatically when replacement is proven."
	}
	return &status
}

// AwaitAdmission pauses new work while profile verification is unavailable.
// Existing workers retain their contexts and cleanup/publication authority.
func (controller *Controller) AwaitAdmission(ctx context.Context) error {
	status := controller.ControllerProfileStatus()
	if status == nil || status.State == "verified" {
		return ctx.Err()
	}
	_, err := controller.verifyNonrootWithRetry(ctx, Request{}, true)
	return err
}

func waitProfileRetry(ctx context.Context, retryAt time.Time) error {
	if delay := time.Until(retryAt); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	return ctx.Err()
}

func (controller *Controller) verifyNonrootWithRetry(ctx context.Context, request Request, waitInvalid bool) (*nonrootProof, error) {
	for {
		if err := waitProfileRetry(ctx, controller.ControllerProfileStatus().RetryAt); err != nil {
			return nil, err
		}
		// Serialize attempts, including their state update. Every request still
		// obtains its own fresh proof; a prior successful proof is never reused.
		controller.profileVerifyMu.Lock()
		if err := ctx.Err(); err != nil {
			controller.profileVerifyMu.Unlock()
			return nil, err
		}
		if time.Until(controller.ControllerProfileStatus().RetryAt) > 0 {
			controller.profileVerifyMu.Unlock()
			continue
		}
		attemptCtx, cancel := context.WithTimeout(ctx, profileReadTimeout)
		proof, err := verifyNonrootController(attemptCtx, controller.kube, controller.config.Namespace, request)
		cancel()
		if ctx.Err() != nil {
			controller.profileVerifyMu.Unlock()
			return nil, ctx.Err()
		}
		controller.profileMu.Lock()
		status := &controller.profileStatus
		var readErr *profileReadError
		isReadError := errors.As(err, &readErr)
		if err == nil {
			status.State, status.Object, status.ErrorClass, status.Reason = "verified", "", "", ""
			controller.profileLogAt = time.Time{}
			status.RetryAt = time.Time{}
			controller.profileBackoff = 0
		} else {
			previousReason := status.Reason
			status.State, status.Object, status.ErrorClass = "blocked", "controller-profile", "invalid_profile"
			// Validation errors contain only fixed verifier predicates, never API bodies.
			status.Reason = err.Error()
			if isReadError {
				status.Reason = readErr.message
				status.ReadFailuresTotal++
				status.Object, status.ErrorClass = readErr.object, profileReadClass(readErr.cause)
			}
			controller.profileBackoff = min(profileRetryMaximum, max(profileRetryMinimum, controller.profileBackoff*2))
			status.RetryAt = time.Now().Add(controller.profileBackoff)
			if previousReason != status.Reason || time.Since(controller.profileLogAt) >= profileLogInterval {
				log.Printf("controller profile unverified: object=%q class=%s reason=%q admission=paused retry_in=%s read_failures_total=%d", status.Object, status.ErrorClass, status.Reason, controller.profileBackoff, status.ReadFailuresTotal)
				controller.profileLogAt = time.Now()
			}
		}
		controller.profileMu.Unlock()
		controller.profileVerifyMu.Unlock()
		if err == nil || (!isReadError && !waitInvalid) {
			return proof, err
		}
	}
}
