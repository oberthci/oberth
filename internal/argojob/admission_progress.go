package argojob

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/oberthci/oberth/pkg/argoworkflow"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

const DefaultStalledRunTimeout = 10 * time.Minute
const admissionObservationTimeout = 2 * time.Minute
const executorReadinessTimeout = 2 * time.Minute
const executorIdentityRetryAnnotation = "oberth.ci/executor-identity-retry-at"

type admissionProgress struct {
	maxPodCPU                int64
	lastProgress             time.Time
	scheduled                bool
	identity                 map[types.UID]identityWait
	readFailingSince         time.Time
	identityReadFailingSince time.Time
	unobservedIdentity       map[types.UID]time.Time
}
type identityWait struct {
	since     time.Time
	retried   bool
	persisted bool
}

type admissionProgressError struct {
	reason   string
	identity bool
}

func (e *admissionProgressError) AdmissionExpiryReason() string {
	if e.identity {
		return "identity"
	}
	return "stalled"
}

func (e *admissionProgressError) Error() string             { return e.reason }
func (*admissionProgressError) InfrastructureFailure() bool { return true }

func (p *admissionProgress) observeIdentityReads(unavailable bool, now time.Time) error {
	if !unavailable {
		p.identityReadFailingSince = time.Time{}
		return nil
	}
	if p.identityReadFailingSince.IsZero() {
		p.identityReadFailingSince = now
	}
	if now.Sub(p.identityReadFailingSince) >= admissionObservationTimeout {
		return &admissionProgressError{reason: "Kubernetes executor identity observation unavailable for 2m", identity: true}
	}
	return nil
}

// The identity holder is intentionally never scheduled. It binds the token's
// lifetime to the owned Workflow and is deleted during workflow cleanup.
// Removing its gate would run an unused holder container. Readiness belongs
// to the real step's token waiter.
func (p *admissionProgress) check(wf *wfv1.Workflow, pods []corev1.Pod, delivered map[types.UID]bool, now time.Time, timeout time.Duration) error {
	if wf.Status.Phase.Completed() {
		return nil
	}
	if timeout <= 0 {
		timeout = DefaultStalledRunTimeout
	}
	if p.lastProgress.IsZero() {
		p.lastProgress = wf.CreationTimestamp.Time
		if wf.Status.StartedAt.After(p.lastProgress) {
			p.lastProgress = wf.Status.StartedAt.Time
		}
		if p.lastProgress.IsZero() || p.lastProgress.After(now) {
			p.lastProgress = now
		}
	}
	for _, node := range wf.Status.Nodes {
		// Pod-node evidence survives Pod GC and observer restarts. Step-group
		// state alone cannot establish that Kubernetes scheduled a workload.
		if node.Type == wfv1.NodeTypePod && (node.HostNodeName != "" || node.Phase == wfv1.NodeSucceeded) {
			p.scheduled = true
		}
		// Pending/Running node creation alone is not proof of a scheduled pod.
		if node.Phase.Fulfilled(nil) && node.FinishedAt.After(p.lastProgress) && !node.FinishedAt.After(now) {
			p.lastProgress = node.FinishedAt.Time
		}
	}
	capacityWait := waitingForSynchronization(wf)
	live := map[types.UID]bool{}
	unobserved := map[types.UID]bool{}
	if p.identity == nil {
		p.identity = map[types.UID]identityWait{}
	}
	if p.unobservedIdentity == nil {
		p.unobservedIdentity = map[types.UID]time.Time{}
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
		assigned := pod.Spec.NodeName != ""
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionTrue {
				assigned = true
			}
		}
		p.scheduled = p.scheduled || assigned
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if waitingForKubernetesCapacity(pod) && p.maxPodCPU > 0 && argoworkflow.PodCPURequests(pod) > p.maxPodCPU {
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse && strings.Contains(condition.Message, "Insufficient cpu") {
					since := pod.CreationTimestamp.Time
					if !condition.LastTransitionTime.IsZero() {
						since = condition.LastTransitionTime.Time
					}
					if !since.IsZero() && !since.After(now) && now.Sub(since) >= timeout {
						return &admissionProgressError{reason: fmt.Sprintf("pod CPU request %dm exceeds largest eligible node %dm for %s; reduce pod requests", argoworkflow.PodCPURequests(pod), p.maxPodCPU, timeout)}
					}
				}
			}
		}
		capacityWait = capacityWait || waitingForKubernetesCapacity(pod)
		running, complete := identityWaiterState(pod)
		if assigned && !running && !complete && !delivered[pod.UID] {
			for _, container := range pod.Spec.InitContainers {
				if container.Name != executorTokenWaiter {
					continue
				}
				unobserved[pod.UID] = true
				since := p.unobservedIdentity[pod.UID]
				if since.IsZero() {
					since = now
					// Charge time since scheduling, never time spent Pending for capacity.
					for _, condition := range pod.Status.Conditions {
						if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionTrue && !condition.LastTransitionTime.IsZero() && !condition.LastTransitionTime.After(now) {
							since = condition.LastTransitionTime.Time
						}
					}
					p.unobservedIdentity[pod.UID] = since
				}
				if now.Sub(since) >= timeout {
					return &admissionProgressError{reason: fmt.Sprintf("executor identity readiness not observed for %s after pod scheduling", timeout), identity: true}
				}
			}
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
						return &admissionProgressError{reason: "executor identity retry deadline is invalid", identity: true}
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
					return &admissionProgressError{reason: "executor identity readiness deadline exceeded after one issuance retry", identity: true}
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
	for uid := range p.unobservedIdentity {
		if !unobserved[uid] {
			delete(p.unobservedIdentity, uid)
		}
	}
	if p.scheduled || capacityWait {
		p.lastProgress = now
		return nil
	}
	if now.Sub(p.lastProgress) >= timeout {
		return &admissionProgressError{reason: fmt.Sprintf("resource admission expired: no scheduled pod or progress for %s", timeout)}
	}
	return nil
}

// waitingForLock recognizes the Argo controller's synchronization wait,
// "Waiting for <lock> lock. Lock status: <free>/<limit>", on a Workflow or a
// node. The controller holds the whole run (workflow-level) or the step
// (template-level) in Pending until the semaphore or mutex frees; nothing of
// Oberth's is stalled, so the startup clock does not run.
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

// postponedByParallelism reports a Workflow the Argo controller holds back
// for its parallelism limit. Like a workflow-level lock wait it has not
// started by Argo's clock, so neither Oberth's startup bound nor its
// watchdog counts the wait.
func postponedByParallelism(wf *wfv1.Workflow) bool {
	return wf != nil && wf.Status.Phase == wfv1.WorkflowPending && wf.Status.StartedAt.IsZero() &&
		wf.Status.Message == argoParallelismPostponed
}

// waitingForSynchronization reports a Workflow queued by the Argo controller:
// postponed for its parallelism limit, or behind native synchronization (the
// Workflow itself, before Argo records its start, or any Pending node). Argo
// anchors its own deadline to startedAt, so a postponed or workflow-level
// wait burns nothing; a template-level wait burns the workflow deadline Argo
// enforces, and Oberth adds no clock of its own on top.
func waitingForSynchronization(wf *wfv1.Workflow) bool {
	if wf == nil || wf.Status.Phase.Completed() {
		return false
	}
	if postponedByParallelism(wf) || waitingForLock(wf.Status.Message) {
		return true
	}
	for _, node := range wf.Status.Nodes {
		if node.Phase == wfv1.NodePending && waitingForLock(node.Message) {
			return true
		}
	}
	return false
}

// Only the scheduler's recognized capacity condition suspends the startup
// clock. Scheduling gates, placement constraints and foreign Pods do not.
func waitingForKubernetesCapacity(pod *corev1.Pod) bool {
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

func (controller *Controller) checkAdmissionProgress(ctx context.Context, wf *wfv1.Workflow, progress *admissionProgress, delivered map[types.UID]bool) error {
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
		if time.Since(progress.readFailingSince) >= admissionObservationTimeout {
			return &admissionProgressError{reason: "Kubernetes pod observation unavailable for 2m"}
		}
		return nil
	}
	progress.readFailingSince = time.Time{}
	progress.maxPodCPU = controller.config.CurrentCPU(ctx).NodeMilli
	err = progress.check(wf, pods.Items, delivered, time.Now(), controller.config.stalledRunTimeout())
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

func (config Config) stalledRunTimeout() time.Duration {
	if config.StalledRunTimeout > 0 {
		return config.StalledRunTimeout
	}
	return DefaultStalledRunTimeout
}
