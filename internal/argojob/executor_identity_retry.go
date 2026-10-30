package argojob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
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
}

func (err *executorIdentityError) Error() string {
	return fmt.Sprintf("deliver executor identity failed (stage=%s reason=%s attempts=%d)", err.stage, err.reason, err.attempts)
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

func identityFailure(stage string, err error) error {
	reason := executorIdentityErrorClass(err)
	if stage == "target_validation" {
		reason = "invalid_target"
	}
	if stage == "token_response" || stage == "trust_anchor_response" {
		reason = "invalid_response"
	}

	return &executorIdentityError{stage: stage, reason: reason, attempts: 1, cause: err}
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

func (controller *Controller) execExecutorIdentity(ctx context.Context, workflow *wfv1.Workflow, original *corev1.Pod, launcher bool, payload []byte) error {
	for attempt := 1; attempt <= executorIdentityAttempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, executorIdentityAttemptTimeout)
		started := time.Now()
		pod, err := controller.kube.CoreV1().Pods(workflow.Namespace).Get(attemptCtx, original.Name, metav1.GetOptions{})
		stage, reason, retry := "pod_read", "", false
		if err != nil {
			retry = retryExecutorIdentityRead(err)
		} else if pod.UID == "" || original.UID == "" || pod.UID != original.UID || !workflowOwnsIdentityTarget(workflow, pod) {
			stage = "target_validation"
			err, reason = errors.New("executor identity target changed"), "target_changed"
		} else if err = validateExecutorTokenTarget(pod, controller.config.SourceSeedImage, launcher); err != nil {
			stage, reason = "target_validation", "invalid_target"
		} else {
			stage = "exec"
			running, complete := false, false
			for _, status := range pod.Status.InitContainerStatuses {
				if status.Name != executorTokenWaiter {
					continue
				}
				running = status.State.Running != nil
				complete = status.State.Terminated != nil && status.State.Terminated.ExitCode == 0
			}
			if complete {
				// The verified waiter's exact command can complete only after the token
				// marker exists. A previous exec committed but lost its acknowledgement.
				cancel()
				return nil
			}
			if !running {
				err, reason, retry = errors.New("executor waiter not running"), "waiter_not_running", false
			} else {
				err = controller.seeder.exec(attemptCtx, workflow.Namespace, pod.Name, executorTokenWaiter,
					[]string{"sh", "-c", executorIdentityDeliveryCommand}, bytes.NewReader(payload), io.Discard, io.Discard)
				retry = retryExecutorIdentityExec(err)
			}
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
		log.Printf("executor_identity: workflow=%s pod=%s uid=%s stage=%s reason=%s attempt=%d/%d elapsed=%s", strconv.Quote(workflow.Name), strconv.Quote(original.Name), strconv.Quote(string(original.UID)), stage, reason, attempt, executorIdentityAttempts, time.Since(started))
		if ctx.Err() != nil || !retry || attempt == executorIdentityAttempts {
			return failure
		}
		timer := time.NewTimer(time.Duration(1<<(attempt-1)) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return &executorIdentityError{stage: "retry_wait", reason: executorIdentityErrorClass(ctx.Err()), attempts: attempt, cause: ctx.Err()}
		case <-timer.C:
		}
	}
	panic("unreachable executor identity retry")
}
