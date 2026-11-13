package argojob

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/model"
)

const maxSchedulingItems = 16

// Seed diagnostics remain live until source delivery removes them. They must
// never inherit the cluster client timeout on a synchronous run-list read.
const seedObservationTimeout = 100 * time.Millisecond

// ObserveScheduling reads shared Workflow and step-Pod caches. A missing
// Workflow may use the bounded, exact source-seed diagnostic read.
// AI-CONTRACT: arbitrary Kubernetes/Argo status text is untrusted: output only
// fixed summaries, never raw messages (which may contain secrets or commands).
func (controller *Controller) ObserveScheduling(ctx context.Context, name, runID, sha string) (*model.ExecutionObservation, error) {
	if controller.hub == nil || !controller.hub.Healthy() {
		return controller.observationUnavailable(), nil
	}
	workflow, found := controller.hub.Workflow(name)
	if !found {
		// Source seed Pods have a separate, short-lived observation path (S1 row
		// 10); they are deliberately outside the step-Pod informer selector.
		if controller.seeder != nil {
			seedCtx, cancel := context.WithTimeout(ctx, seedObservationTimeout)
			observation, err := controller.seeder.observeScheduling(seedCtx, name, runID, sha)
			timedOut := errors.Is(seedCtx.Err(), context.DeadlineExceeded)
			cancel()
			if timedOut {
				return controller.observationUnavailable(), nil
			}
			if err != nil || observation != nil {
				return observation, err
			}
		}
		return &model.ExecutionObservation{State: "not_observed", Message: "No Workflow was observed; startup or scheduling cause is unknown."}, nil
	}
	if workflow.Name != name || workflow.Namespace != controller.config.Namespace ||
		workflow.Annotations[runIDAnnotation] != runID || workflow.Labels["oberth.ci/sha"] != labelValue(sha) {
		return nil, errors.New("argojob: scheduling observation identity mismatch")
	}
	result := &model.ExecutionObservation{State: "observed", Phase: safePhase(string(workflow.Status.Phase)), Message: "Workflow observed; no step execution progress has been recorded."}
	if workflow.Status.Message != "" {
		result.Message = safeWorkflowMessage(workflow.Status.Message)
	}
	if postponedByParallelism(workflow) {
		result.Message = model.ParallelismWaitMessage
	}
	if workflow.Status.Message == "" && workflow.Status.Phase == wfv1.WorkflowRunning {
		result.Message = "Workflow execution is running."
	}
	// Suspension is observed from the cached Workflow, never inferred from
	// missing progress or copied from arbitrary controller messages.
	defer func() {
		if workflow.Status.Phase == wfv1.WorkflowSucceeded || workflow.Status.Phase == wfv1.WorkflowFailed || workflow.Status.Phase == wfv1.WorkflowError {
			return
		}
		if workflow.Spec.Suspend != nil && *workflow.Spec.Suspend {
			result.Reason, result.Message = "Suspended", model.WorkflowSuspendedMessage
			return
		}
		for _, node := range workflow.Status.Nodes {
			if node.Type == wfv1.NodeTypeSuspend && node.Phase == wfv1.NodeRunning {
				result.Reason, result.Message = "Suspended", model.StepSuspendedMessage
				return
			}
		}
	}()
	now := time.Now().UTC()
	observeWorkflowDeadline(workflow, result, now)

	// Sort only a bounded sample using insertion, avoiding an allocation for every
	// node in a large Workflow. No node name is returned: expanded names can carry
	// user-supplied parameter values.
	ids := make([]string, 0, maxSchedulingItems)
	for id, node := range workflow.Status.Nodes {
		if node.Phase != wfv1.NodePending && node.Phase != wfv1.NodeRunning && node.Phase != wfv1.NodeError && node.Phase != wfv1.NodeFailed {
			continue
		}
		i := sort.SearchStrings(ids, id)
		if len(ids) == maxSchedulingItems {
			result.Truncated = true
			if i == maxSchedulingItems {
				continue
			}
			ids = ids[:maxSchedulingItems-1]
		}
		ids = append(ids, "")
		copy(ids[i+1:], ids[i:])
		ids[i] = id
	}
	for _, id := range ids {
		node := workflow.Status.Nodes[id]
		item := model.SchedulingReason{Phase: safePhase(string(node.Phase))}
		if node.Message != "" {
			item.Message = safeWorkflowMessage(node.Message)
		}
		result.Nodes = append(result.Nodes, item)
	}
	if controller.kube == nil || workflow.UID == "" {
		result.Pods = []model.SchedulingReason{{Phase: "Unknown", Reason: "Unavailable", Message: "Pod observation is unavailable."}}
		return result, nil
	}
	pods := controller.hub.PodsOf(name)
	result.Truncated = result.Truncated || len(pods) > maxSchedulingItems
	var deadlineNodes map[string]bool
	if deadlineVerdictVisible(workflow, result) {
		deadlineNodes = currentDeadlineNodes(workflow)
	}
	for _, pod := range pods[:min(len(pods), maxSchedulingItems)] {
		owner := metav1.GetControllerOf(pod)
		if owner == nil || owner.UID != workflow.UID || owner.Name != name || owner.Kind != "Workflow" || owner.APIVersion != "argoproj.io/v1alpha1" {
			continue
		}
		reason := podSchedulingReason(*pod)
		result.Pods = append(result.Pods, reason)
		if reason.Reason == "Unschedulable" && (podCapacityPending(pod) || schedulerPlacementReason(*pod) != "") {
			result.Message = reason.Message
		}
		if reason.Reason == "DeadlineExceeded" && deadlineNodes[pod.Annotations["workflows.argoproj.io/node-id"]] {
			markDeadlineExceeded(result)
		}
	}
	return result, nil
}

func safePhase(phase string) string {
	switch phase {
	case "Pending", "Running", "Succeeded", "Failed", "Error", "Skipped", "Omitted":
		return phase
	default:
		return "Unknown"
	}
}

func safeWorkflowMessage(message string) string {
	// Only recognize controller vocabulary. Even a recognized message contributes
	// no original bytes to the response: resource names and error suffixes can be
	// user-controlled and are intentionally withheld.
	switch {
	case strings.HasPrefix(message, "Waiting for a PVC to be created."):
		return "Waiting for a persistent volume claim to be created."
	case waitingForLock(message):
		return lockWaitMessage(message)
	case strings.HasPrefix(message, "Pending "):
		return "Workflow node is pending."
	case model.IsExecutionDeadlineReason(message):
		return model.ExecutionDeadlineMessage
	case message == "Pod was deleted":
		return "Workflow pod was deleted."
	default:
		return "Controller phase message is present; free-form details withheld."
	}
}

func podSchedulingReason(pod corev1.Pod) (result model.SchedulingReason) {
	defer func() {
		if pod.Status.Phase == corev1.PodPending && result.Message != "" && !strings.HasPrefix(result.Message, "pod pending: ") {
			result.Message = "pod pending: " + result.Message
		}
	}()
	result = model.SchedulingReason{Phase: safePhase(string(pod.Status.Phase))}
	if pod.Status.Reason == "DeadlineExceeded" {
		result.Reason, result.Message = "DeadlineExceeded", model.ExecutionDeadlineMessage
		return result
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse {
			result.Reason = "Unscheduled"
			result.Message = "not scheduled"
			if condition.Reason == "Unschedulable" {
				result.Reason = "Unschedulable"
				switch {
				case strings.Contains(condition.Message, "Insufficient cpu") && strings.Contains(condition.Message, "Insufficient memory"):
					result.Message = "pod pending: insufficient cpu and memory (" + podRequest(pod, corev1.ResourceCPU) + " CPU, " + podRequest(pod, corev1.ResourceMemory) + " requested)"
				case strings.Contains(condition.Message, "Insufficient cpu"):
					result.Message = "pod pending: insufficient cpu (" + podRequest(pod, corev1.ResourceCPU) + " CPU requested)"
				case strings.Contains(condition.Message, "Insufficient memory"):
					result.Message = "pod pending: insufficient memory (" + podRequest(pod, corev1.ResourceMemory) + " requested)"
				case hasSchedulingPhrase(condition.Message, "Insufficient", "ephemeral-storage"):
					result.Message = "insufficient ephemeral-storage; pod requests " + podRequest(pod, corev1.ResourceEphemeralStorage) + " ephemeral storage."
				case hasSchedulingPhrase(condition.Message, "Too", "many", "pods"):
					result.Message = "too many pods; waiting for a pod slot"
				case strings.Contains(condition.Message, "unbound immediate PersistentVolumeClaims"):
					result.Message = "unbound persistent volume claim"
				}
				if placement := schedulerPlacementReason(pod); placement != "" {
					if result.Message == "not scheduled" {
						result.Message = placement
					} else {
						result.Message += "; " + placement
					}
				}
			}
			return result
		}
	}
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, status := range statuses {
			if status.State.Waiting == nil {
				continue
			}
			result.Reason = safeWaitingReason(status.State.Waiting.Reason)
			result.Message = "container is waiting to start; free-form details withheld"
			return result
		}
	}
	if pod.Status.Phase == corev1.PodPending {
		result.Reason = "Pending"
		result.Message = "no more specific scheduling reason was observed"
	}
	return result
}

// Match whole scheduler words, allowing sentence punctuation after the reason.
// Prefix lookalikes must not become capacity reasons or suspend a deadline.
func hasSchedulingPhrase(message string, words ...string) bool {
	fields := strings.Fields(message)
	for i := 0; i+len(words) <= len(fields); i++ {
		matched := true
		for j, word := range words {
			field := fields[i+j]
			if j == len(words)-1 {
				field = strings.TrimRight(field, ".,;:")
			}
			if field != word {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func safeWaitingReason(reason string) string {
	switch reason {
	case "ContainerCreating", "PodInitializing", "ErrImagePull", "ImagePullBackOff", "CrashLoopBackOff", "CreateContainerConfigError", "CreateContainerError", "RunContainerError", "InvalidImageName":
		return reason
	default:
		return "Waiting"
	}
}

// podRequest is the scheduler's effective request for one resource: the sum
// of the app containers' requests or the largest init container's request,
// whichever is larger. Only the quantity is reported: it is a number from
// the admitted spec, never free-form text (#024).
func podRequest(pod corev1.Pod, name corev1.ResourceName) string {
	total := resource.Quantity{}
	for _, container := range pod.Spec.Containers {
		if request, ok := container.Resources.Requests[name]; ok {
			total.Add(request)
		}
	}
	for _, container := range pod.Spec.InitContainers {
		if request, ok := container.Resources.Requests[name]; ok && request.Cmp(total) > 0 {
			total = request.DeepCopy()
		}
	}
	if total.IsZero() {
		return "no explicit"
	}
	return total.String()
}

// lockWaitPattern is the controller's synchronization wait with its lock key
// (<namespace>/<Kind>/<name>[/<key>], Kind one of ConfigMap, Mutex, Database)
// and "free/limit" status. Only a key in this grammar reaches the message,
// without its namespace; anything else gets the fixed form.
var lockWaitPattern = regexp.MustCompile(`^Waiting for [A-Za-z0-9][A-Za-z0-9_.-]{0,62}/((?:ConfigMap|Mutex|Database)/[A-Za-z0-9][A-Za-z0-9_.-]{0,252}(?:/[A-Za-z0-9][A-Za-z0-9_.-]{0,252})?) lock\. Lock status: ([0-9]{1,6})/([0-9]{1,6})`)

// lockWaitMessage renders Argo's "Waiting for <ns>/<Kind>/<name>[/<key>]
// lock. Lock status: a/b" as "Waiting for lock <Kind>/<name>[/<key>] (a/b
// free).", naming the operator-defined semaphore key or the repository's
// mutex so the reason line says what the step is queued behind.
func lockWaitMessage(message string) string {
	match := lockWaitPattern.FindStringSubmatch(message)
	if match == nil {
		return model.LockWaitMessagePrefix + "(synchronization)."
	}
	return fmt.Sprintf("%s%s (%s/%s free).", model.LockWaitMessagePrefix, match[1], match[2], match[3])
}
