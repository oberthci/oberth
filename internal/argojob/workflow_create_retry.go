package argojob

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const workflowCreateAttempts = 3
const workflowCreateBudget = time.Minute

// Preserve the cause for classification without exposing arbitrary API bodies.
// A caller must not infer absence from this error: a timed-out POST can commit
// later even when a subsequent GET returned NotFound.
type workflowCreateFailure struct {
	name, stage string
	attempts    int
	cause       error
	infra       bool
}

func (e *workflowCreateFailure) Error() string {
	return fmt.Sprintf("argojob: create Workflow %s failed (stage=%s reason=%s attempts=%d)", e.name, e.stage, executorIdentityErrorClass(e.cause), e.attempts)
}
func (e *workflowCreateFailure) Unwrap() error               { return e.cause }
func (e *workflowCreateFailure) InfrastructureFailure() bool { return e.infra }

func transientWorkflowCreate(err error) bool {
	return !errors.Is(err, context.Canceled) && retryExecutorIdentityRead(err)
}

func definiteWorkflowRejection(err error) bool {
	return apierrors.IsInvalid(err) || apierrors.IsForbidden(err) || apierrors.IsBadRequest(err)
}

// createWorkflowWithRetry never seeds again or changes the intended name/spec.
// Each repeated POST requires a fresh NotFound after backoff. An uncertain GET
// retries observation only; a foreign Workflow or changed profile stops hard.
// rejected permits source cleanup only after a definitive rejection with no
// earlier ambiguous POST. In particular, exhaustion must retain the claim.
func (controller *Controller) createWorkflowWithRetry(ctx context.Context, request Request, intended *wfv1.Workflow) (*wfv1.Workflow, bool, error) {
	retryCtx, cancel := context.WithTimeout(ctx, workflowCreateBudget)
	defer cancel()
	attempts, waits := 0, 0
	reconcile, backedOff, ambiguous, mayRetry := false, false, false, false
	var createErr, lastErr error
	failure := func(stage string, cause error, infra bool) (*wfv1.Workflow, bool, error) {
		return nil, false, &workflowCreateFailure{name: intended.Name, stage: stage, attempts: attempts, cause: cause, infra: infra}
	}
	wait := func(stage string, cause error) error {
		waits++
		delay := executorIdentityBackoff(min(waits, 2))
		if seconds, suggested := apierrors.SuggestsClientDelay(cause); suggested {
			delay = max(delay, time.Duration(seconds)*time.Second)
		}
		log.Printf("workflow_create: workflow=%q stage=%s reason=%s post_attempts=%d/%d retry_in=%s", intended.Name, stage, executorIdentityErrorClass(cause), attempts, workflowCreateAttempts, delay)
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-retryCtx.Done():
			return retryCtx.Err()
		case <-timer.C:
			return nil
		}
	}
	for {
		if err := retryCtx.Err(); err != nil {
			return failure("deadline", errors.Join(lastErr, err), true)
		}
		if reconcile {
			existing, err := controller.workflows.Get(retryCtx, intended.Name, metav1.GetOptions{})
			if err == nil {
				if _, err := controller.refreshNonroot(retryCtx, request); err != nil {
					return nil, false, err
				}
				if err := sameSubmission(existing, intended); err != nil {
					return nil, false, err
				}
				return existing, false, nil
			}
			if !apierrors.IsNotFound(err) {
				lastErr = err
				if !mayRetry || !transientWorkflowCreate(err) {
					return failure("reconcile", err, transientWorkflowCreate(err))
				}
				if err := wait("reconcile", err); err != nil {
					return failure("deadline", errors.Join(lastErr, err), true)
				}
				continue // Never POST while the outcome of GET is unknown.
			}
			if !mayRetry || attempts >= workflowCreateAttempts {
				return nil, !ambiguous && definiteWorkflowRejection(createErr), &workflowCreateFailure{name: intended.Name, stage: "create", attempts: attempts, cause: createErr, infra: mayRetry}
			}
			if !backedOff {
				if err := wait("create", createErr); err != nil {
					return failure("deadline", errors.Join(createErr, err), true)
				}
				backedOff = true
				continue // Re-read after backoff: the old POST may have committed.
			}
		}
		if attempts > 0 {
			if _, err := controller.refreshNonroot(retryCtx, request); err != nil {
				return nil, false, err
			}
		}
		if err := retryCtx.Err(); err != nil {
			return failure("deadline", errors.Join(lastErr, err), true)
		}
		created, err := controller.workflows.Create(retryCtx, intended.DeepCopy(), metav1.CreateOptions{})
		attempts++
		if err == nil {
			return created, false, nil
		}
		createErr, lastErr = err, err
		mayRetry = transientWorkflowCreate(err) || apierrors.IsAlreadyExists(err)
		ambiguous = ambiguous || (!definiteWorkflowRejection(err) && !apierrors.IsAlreadyExists(err))
		reconcile, backedOff = true, false
	}
}
