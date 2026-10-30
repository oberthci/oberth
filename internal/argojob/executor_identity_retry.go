package argojob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"strconv"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kexec "k8s.io/client-go/util/exec"
)

const executorIdentityAttempts = 3
const executorIdentityAttemptTimeout = 20 * time.Second

// Commit the token marker last, using a no-clobber hard link. Each concurrent
// or timed-out attempt has private staging files. Retried execs use the SAME
// payload; an ambiguous acknowledgement cannot replace a committed identity.
// The CA is a nonempty base64 line terminated by a newline: EOF inside any
// field must fail before publishing the marker. The pinned busybox image
// supplies base64, mktemp, ln, mv and the POSIX shell.
const executorIdentityDeliveryCommand = `set -eu; umask 077; cd /var/run/secrets/kubernetes.io/serviceaccount; test ! -f token || exit 0; stage=$(mktemp -d .identity.XXXXXX); trap 'rm -rf "$stage"' EXIT; IFS= read -r token; IFS= read -r namespace; IFS= read -r ca; test -n "$token"; test -n "$namespace"; test -n "$ca"; printf '%s' "$ca" | base64 -d > "$stage/ca.crt"; test -s "$stage/ca.crt"; printf '%s' "$namespace" > "$stage/namespace"; printf '%s' "$token" > "$stage/token"; chmod 0444 "$stage/ca.crt" "$stage/namespace" "$stage/token"; mv "$stage/ca.crt" ca.crt; mv "$stage/namespace" namespace; ln "$stage/token" token 2>/dev/null || test -f token`

// The cause is available to errors.Is/As but is never interpolated into public
// errors or logs: transport errors can contain arbitrary server response text.
type executorIdentityError struct {
	stage    string
	reason   string
	attempts int
	cause    error
	public   string
}

func (err *executorIdentityError) Error() string {
	text := fmt.Sprintf("deliver executor identity failed (stage=%s reason=%s attempts=%d)", err.stage, err.reason, err.attempts)
	if err.public != "" {
		text += ": " + err.public
	}
	return text
}
func (err *executorIdentityError) Unwrap() error { return err.cause }
func (err *executorIdentityError) InfrastructureFailure() bool {
	switch err.stage {
	case "pod_list", "pod_read", "identity_pod", "token_request", "trust_anchor", "exec", "retry_wait":
		switch err.reason {
		case "deadline", "transport_timeout", "transport_eof", "transport", "throttled", "api_timeout", "api_unavailable", "api_internal":
			return true
		}
	}
	return false
}

func identityFailure(stage string, err error) *executorIdentityError {
	reason := executorIdentityErrorClass(err)
	if stage == "target_validation" {
		reason = "invalid_target"
	}
	if stage == "token_response" || stage == "trust_anchor_response" {
		reason = "invalid_response"
	}

	failure := &executorIdentityError{stage: stage, reason: reason, attempts: 1, cause: err}
	if stage == "launcher_provision" && errors.Is(err, errVMIOverGrant) {
		failure.reason, failure.public = "vmi_over_grant", errVMIOverGrant.Error()
	}
	if stage == "target_validation" || stage == "identity_pod" {
		switch err.Error() {
		case "executor token target is not memory-backed", "ephemeral container can access executor identity", "executor identity waiter was modified", "pipeline container can access executor identity", "executor identity waiter missing", "executor identity target owner changed", "executor identity target has no UID", "executor identity target changed during exec", "executor identity lifetime Pod has unexpected ownership or configuration", "executor identity owner changed":
			failure.public = err.Error() // Exact allowlist of fixed internal diagnostics.
		}
	}
	return failure
}

func executorIdentityErrorClass(err error) string {
	var exit kexec.ExitError
	var network net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case apierrors.IsForbidden(err):
		return "forbidden"
	case apierrors.IsUnauthorized(err):
		return "unauthorized"
	case apierrors.IsTooManyRequests(err):
		return "throttled"
	case apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err):
		return "api_timeout"
	case apierrors.IsServiceUnavailable(err):
		return "api_unavailable"
	case apierrors.IsInternalError(err):
		return "api_internal"
	case apierrors.IsNotFound(err):
		return "not_found"
	case apierrors.IsInvalid(err) || apierrors.IsBadRequest(err):
		return "invalid_request"
	case errors.As(err, &exit):
		return "remote_exit"
	case errors.As(err, &network) && network.Timeout():
		return "transport_timeout"
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return "transport_eof"
	case errors.As(err, &network):
		return "transport"
	default:
		return "unknown"
	}
}

func retryExecutorIdentityExec(err error) bool {
	switch executorIdentityErrorClass(err) {
	case "canceled", "forbidden", "unauthorized", "invalid_request", "remote_exit", "not_found":
		return false
	default:
		return true // Includes upgrade/startup races and lost acknowledgements.
	}
}

func retryExecutorIdentityRead(err error) bool {
	var network net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsInternalError(err) || errors.As(err, &network)
}

func workflowOwnsIdentityTarget(workflow *wfv1.Workflow, pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return false
	}
	for _, ref := range pod.OwnerReferences {
		if ref.UID == workflow.UID && ref.Kind == "Workflow" && ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

// A disappearing target is left to Argo's lifecycle/result observation. The
// delivered map must not cache it; a replacement is validated on the next tick.
var errExecutorIdentityTargetGone = errors.New("executor identity target gone")

func (controller *Controller) executorIdentityTarget(ctx context.Context, workflow *wfv1.Workflow, original *corev1.Pod, launcher bool) (*corev1.Pod, error) {
	pod, err := controller.kube.CoreV1().Pods(workflow.Namespace).Get(ctx, original.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, errExecutorIdentityTargetGone
	}
	if err != nil {
		return nil, err
	}
	if pod.UID == "" || original.UID == "" {
		return nil, identityFailure("target_validation", errors.New("executor identity target has no UID"))
	}
	if pod.UID != original.UID || pod.DeletionTimestamp != nil {
		return nil, errExecutorIdentityTargetGone
	}
	if !workflowOwnsIdentityTarget(workflow, pod) {
		return nil, identityFailure("target_validation", errors.New("executor identity target owner changed"))
	}
	if err := validateExecutorTokenTarget(pod, controller.config.SourceSeedImage, launcher); err != nil {
		return nil, identityFailure("target_validation", err)
	}
	for _, status := range pod.Status.InitContainerStatuses {
		if status.Name == executorTokenWaiter && status.State.Terminated != nil && status.State.Terminated.ExitCode != 0 {
			return nil, errExecutorIdentityTargetGone
		}
	}
	return pod, nil
}

func identityWaiterState(pod *corev1.Pod) (running, complete bool) {
	for _, status := range pod.Status.InitContainerStatuses {
		if status.Name == executorTokenWaiter {
			return status.State.Running != nil, status.State.Terminated != nil && status.State.Terminated.ExitCode == 0
		}
	}
	return false, false
}

// Exec addresses a Pod by name, not UID. Verify again after an acknowledgement;
// on uncertainty retire the bound identity so a replacement cannot keep it.
// A remote exit gets exactly one reconciliation read, never another exec.
func (controller *Controller) reconcileExecutorIdentity(ctx context.Context, workflow *wfv1.Workflow, original *corev1.Pod, launcher bool, execErr error) error {
	pod, err := controller.executorIdentityTarget(ctx, workflow, original, launcher)
	if err == nil {
		_, complete := identityWaiterState(pod)
		if execErr == nil || complete {
			return nil
		}
		return execErr
	}
	var invalid *executorIdentityError
	if execErr != nil && !errors.As(err, &invalid) {
		if errors.Is(err, errExecutorIdentityTargetGone) {
			return err
		}
		return execErr // Unknown acknowledgement remains the original hard exit.
	}
	var failure *executorIdentityError
	if errors.As(err, &invalid) {
		failure = invalid
	} else if errors.Is(err, errExecutorIdentityTargetGone) {
		failure = identityFailure("target_validation", errors.New("executor identity target changed during exec"))
	} else {
		failure = identityFailure("pod_read", err)
	}
	account := controller.config.ExecutorServiceAccount
	if launcher {
		account = controller.config.VMLauncherServiceAccount
	}
	if retireErr := controller.retireIdentityPod(ctx, workflow, account); retireErr != nil {
		failure.cause = errors.Join(failure.cause, retireErr)
	}
	return failure
}

func (controller *Controller) execExecutorIdentity(ctx context.Context, workflow *wfv1.Workflow, original *corev1.Pod, launcher bool, payload []byte) error {
	for attempt := 1; attempt <= executorIdentityAttempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, executorIdentityAttemptTimeout)
		started := time.Now()
		pod, err := controller.executorIdentityTarget(attemptCtx, workflow, original, launcher)
		stage, reason, retry := "pod_read", "", false
		reconciled := false
		if errors.Is(err, errExecutorIdentityTargetGone) {
			cancel()
			return err
		}
		var invalid *executorIdentityError
		if errors.As(err, &invalid) {
			stage, reason = invalid.stage, invalid.reason
		} else if err != nil {
			retry = retryExecutorIdentityRead(err)
		} else {
			stage = "exec"
			running, complete := identityWaiterState(pod)
			if complete {
				// The exact verified waiter can finish only after the marker exists.
				cancel()
				return nil
			}
			if !running {
				cancel()
				return errExecutorIdentityTargetGone
			}
			// StreamWithContext may return before its stdin copier stops. This private
			// immutable copy is never cleared while a stale copier could still read it.
			err = controller.seeder.exec(attemptCtx, workflow.Namespace, pod.Name, executorTokenWaiter,
				[]string{"sh", "-c", executorIdentityDeliveryCommand}, bytes.NewReader(bytes.Clone(payload)), io.Discard, io.Discard)
			if err == nil || executorIdentityErrorClass(err) == "remote_exit" {
				err = controller.reconcileExecutorIdentity(attemptCtx, workflow, original, launcher, err)
				reconciled = true
				if errors.Is(err, errExecutorIdentityTargetGone) {
					cancel()
					return err
				}
				if errors.As(err, &invalid) {
					invalid.attempts = attempt
					cancel()
					return invalid // Retirement already attempted; do not deliver again.
				}
			}
			retry = retryExecutorIdentityExec(err)
		}
		if errors.Is(attemptCtx.Err(), context.DeadlineExceeded) && err != nil && (retry || errors.Is(err, context.Canceled)) {
			err = errors.Join(err, context.DeadlineExceeded)
			retry = true
		}
		cancel()
		if err == nil {
			if attempt > 1 {
				log.Printf("executor_identity: workflow=%s pod=%s uid=%s state=recovered attempts=%d", strconv.Quote(workflow.Name), strconv.Quote(original.Name), strconv.Quote(string(original.UID)), attempt)
			}
			return nil
		}
		if reason == "" {
			reason = executorIdentityErrorClass(err)
		}
		failure := &executorIdentityError{stage: stage, reason: reason, attempts: attempt, cause: err}
		if invalid != nil {
			failure.public = invalid.public
		}
		log.Printf("executor_identity: workflow=%s pod=%s uid=%s stage=%s reason=%s attempt=%d/%d elapsed=%s", strconv.Quote(workflow.Name), strconv.Quote(original.Name), strconv.Quote(string(original.UID)), stage, reason, attempt, executorIdentityAttempts, time.Since(started))
		if ctx.Err() != nil || !retry || attempt == executorIdentityAttempts {
			if ctx.Err() == nil && stage == "exec" && !reconciled {
				// The final attempt can commit at its deadline. Use a fresh bounded read,
				// under the original caller, before giving up without another exec.
				reconcileCtx, stop := context.WithTimeout(ctx, 2*time.Second)
				checked := controller.reconcileExecutorIdentity(reconcileCtx, workflow, original, launcher, err)
				stop()
				if checked == nil || errors.Is(checked, errExecutorIdentityTargetGone) {
					return checked
				}
				var invalid *executorIdentityError
				if errors.As(checked, &invalid) {
					invalid.attempts = attempt
					return invalid
				}
			}
			return failure
		}
		timer := time.NewTimer(executorIdentityBackoff(attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return &executorIdentityError{stage: "retry_wait", reason: executorIdentityErrorClass(ctx.Err()), attempts: attempt, cause: ctx.Err()}
		case <-timer.C:
		}
	}
	panic("unreachable executor identity retry")
}

// ±50% jitter avoids lockstep retries under shared API/runtime load.
func executorIdentityBackoff(attempt int) time.Duration {
	base := time.Duration(1<<(attempt-1)) * time.Second
	return base/2 + time.Duration(rand.Int64N(int64(base)+1)) //nolint:gosec // Public scheduling jitter, not a security value.
}
