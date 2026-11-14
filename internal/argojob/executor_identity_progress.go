package argojob

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

const identityObservationTimeout = 2 * time.Minute
const executorReadinessTimeout = 2 * time.Minute
const executorIdentityRetryAnnotation = "oberth.ci/executor-identity-retry-at"

type executorIdentityProgress struct {
	identity                 map[types.UID]identityWait
	readFailingSince         time.Time
	identityReadFailingSince time.Time
}
type identityWait struct {
	since     time.Time
	retried   bool
	persisted bool
}

type executorIdentityProgressError struct {
	reason string
}

func (*executorIdentityProgressError) ExecutorIdentityExpired() bool { return true }

func (e *executorIdentityProgressError) Error() string             { return e.reason }
func (*executorIdentityProgressError) InfrastructureFailure() bool { return true }

func (p *executorIdentityProgress) observeIdentityReads(unavailable bool, now time.Time) error {
	if !unavailable {
		p.identityReadFailingSince = time.Time{}
		return nil
	}
	if p.identityReadFailingSince.IsZero() {
		p.identityReadFailingSince = now
	}
	if now.Sub(p.identityReadFailingSince) >= identityObservationTimeout {
		return &executorIdentityProgressError{reason: "Kubernetes executor identity observation unavailable for 2m"}
	}
	return nil
}

// The identity holder is intentionally never scheduled. It binds the token's
// lifetime to the owned Workflow and is deleted during workflow cleanup.
// Removing its gate would run an unused holder container. Readiness belongs
// to the real step's token waiter.
func (p *executorIdentityProgress) check(wf *wfv1.Workflow, pods []corev1.Pod, delivered map[types.UID]bool, now time.Time) error {
	if wf.Status.Phase.Completed() {
		return nil
	}
	live := map[types.UID]bool{}
	if p.identity == nil {
		p.identity = map[types.UID]identityWait{}
	}
	for i := range pods {
		pod := &pods[i]
		owner := metav1.GetControllerOf(pod)
		if owner == nil || owner.Kind != "Workflow" || owner.UID != wf.UID || pod.DeletionTimestamp != nil {
			continue
		}
		if pod.Labels["oberth.ci/role"] == "executor-identity" {
			continue
		}
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, status := range pod.Status.InitContainerStatuses {
			if status.Name != executorTokenWaiter || status.State.Running == nil {
				continue
			}
			live[pod.UID] = true
			wait, found := p.identity[pod.UID]
			if !found {
				if stamp := pod.Annotations[executorIdentityRetryAnnotation]; stamp != "" {
					retryAt, err := time.Parse(time.RFC3339Nano, stamp)
					if err != nil || retryAt.After(now) {
						return &executorIdentityProgressError{reason: "executor identity retry deadline is invalid"}
					}
					wait.since, wait.retried, wait.persisted = retryAt, true, true
				} else {
					wait.since = status.State.Running.StartedAt.Time
					if wait.since.IsZero() || wait.since.After(now) {
						wait.since = now
					}
				}
			}
			if now.Sub(wait.since) >= executorReadinessTimeout {
				if wait.retried {
					return &executorIdentityProgressError{reason: "executor identity readiness deadline exceeded after one issuance retry"}
				}
				// Exec's atomic marker is no-clobber: an ambiguous prior delivery cannot
				// overwrite the committed identity. A retry still revalidates ownership.
				delete(delivered, pod.UID)
				wait.retried, wait.since = true, now
			}
			p.identity[pod.UID] = wait
		}
	}
	for uid := range p.identity {
		if !live[uid] {
			delete(p.identity, uid)
		}
	}
	return nil
}

// waitingForLock recognizes the Argo controller's synchronization wait,
// "Waiting for <lock> lock. Lock status: <free>/<limit>", on a Workflow or a
// node. The controller holds the whole run (workflow-level) or the step
// (template-level) in Pending until the semaphore or mutex frees.
func waitingForLock(message string) bool {
	return strings.HasPrefix(message, "Waiting for ") && strings.Contains(message, " lock. Lock status: ")
}

// argoParallelismPostponed is the exact status message the Argo v4.0.8
// workflow controller writes when its throttler does not admit a new
// Workflow (workflow/controller/controller.go, processNextItem): the phase
// becomes Pending, startedAt stays unset, and no condition is added. The
// controller writes it once, on the Workflow's first pass, and replaces it
// when the Workflow is admitted.
const argoParallelismPostponed = "Workflow processing has been postponed because too many workflows are already running"

// postponedByParallelism recognizes native controller waiting for display.
func postponedByParallelism(wf *wfv1.Workflow) bool {
	return wf != nil && wf.Status.Phase == wfv1.WorkflowPending && wf.Status.StartedAt.IsZero() &&
		wf.Status.Message == argoParallelismPostponed
}

// podCapacityPending recognizes safe capacity phrases for read-only diagnostics.
func podCapacityPending(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodPending {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type != corev1.PodScheduled || condition.Status != corev1.ConditionFalse || condition.Reason != "Unschedulable" {
			continue
		}
		if hasSchedulingPhrase(condition.Message, "Too", "many", "pods") || hasSchedulingPhrase(condition.Message, "Insufficient", "ephemeral-storage") {
			return true
		}
		fields := strings.Fields(condition.Message)
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] != "Insufficient" {
				continue
			}
			name := strings.TrimRight(fields[i+1], ".,;:")
			if name == "cpu" || name == "memory" || (strings.Contains(name, "/") && len(validation.IsQualifiedName(name)) == 0) {
				return true
			}
		}
	}
	return false
}

func (controller *Controller) checkExecutorIdentityProgress(ctx context.Context, wf *wfv1.Workflow, progress *executorIdentityProgress, delivered map[types.UID]bool) error {
	if wf.Status.Phase.Completed() || controller.kube == nil {
		return nil
	}
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	pods, err := controller.kube.CoreV1().Pods(wf.Namespace).List(readCtx, metav1.ListOptions{LabelSelector: "workflows.argoproj.io/workflow=" + wf.Name})
	if err != nil {
		// Failed observations never count as progress. The caller settles failure
		// through ownership-checked workflow deletion before releasing capacity.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if progress.readFailingSince.IsZero() {
			progress.readFailingSince = time.Now()
		}
		if time.Since(progress.readFailingSince) >= identityObservationTimeout {
			return &executorIdentityProgressError{reason: "Kubernetes executor identity Pod observation unavailable for 2m"}
		}
		return nil
	}
	progress.readFailingSince = time.Time{}
	err = progress.check(wf, pods.Items, delivered, time.Now())
	if err != nil {
		return err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		wait, ok := progress.identity[pod.UID]
		if !ok || !wait.retried || wait.persisted {
			continue
		}
		if err := validateExecutorTokenTarget(pod, controller.config.SourceSeedImage, mainMountsExecutorToken(pod)); err != nil {
			return identityFailure("target_validation", err)
		}
		patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"uid": pod.UID, "resourceVersion": pod.ResourceVersion, "annotations": map[string]string{executorIdentityRetryAnnotation: wait.since.UTC().Format(time.RFC3339Nano)}}})
		if err != nil {
			return err
		}
		if _, err = controller.kube.CoreV1().Pods(wf.Namespace).Patch(readCtx, pod.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			return identityFailure("pod_read", err)
		}
		wait.persisted = true
		progress.identity[pod.UID] = wait
	}
	return nil
}

// observeWorkflowIdentities keeps Stop exit handlers supplied with executor
// identities while Terminate releases no newly blocked containers.
func (controller *Controller) observeWorkflowIdentities(ctx context.Context, workflow *wfv1.Workflow, admissionWatch *executorIdentityProgress, delivered map[types.UID]bool) error {
	if workflow.Spec.Shutdown == "" {
		if err := controller.checkExecutorIdentityProgress(ctx, workflow, admissionWatch, delivered); err != nil {
			return err
		}
	}
	// Stop still needs executor tokens for exit handlers, even after the
	// original startup deadline. Terminate creates no exit work and must
	// not release newly blocked publish containers while Argo kills them.
	if workflow.Spec.Shutdown != wfv1.ShutdownStrategyTerminate {
		// Bound reads and identity transport even when no waiter observation exists.
		deliveryCtx, stopDelivery := context.WithTimeout(ctx, identityObservationTimeout)
		identityReadUnavailable := false
		deliveryErr := controller.deliverExecutorTokensObserved(deliveryCtx, workflow, delivered, func() { identityReadUnavailable = true })
		if deliveryCtx.Err() != nil && ctx.Err() == nil {
			deliveryErr = &executorIdentityProgressError{reason: "Kubernetes executor identity observation unavailable for 2m"}
		}
		stopDelivery()
		if deliveryErr == nil {
			deliveryErr = admissionWatch.observeIdentityReads(identityReadUnavailable, time.Now())
		}
		if err := deliveryErr; err != nil {
			// Preserve observed execution evidence; a delivery error on a later
			// Pod must not make already executed steps look like a pre-step failure.
			return err
		}
	}
	return nil
}
