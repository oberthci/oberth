package argojob

import (
	"context"
	"errors"
	"sort"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/model"
)

const maxSchedulingItems = 16

// ObserveScheduling performs only an exact Workflow GET and one scoped, bounded
// Pod LIST. It never submits, repairs, reads logs, events, or pod specs for output.
// AI-CONTRACT: arbitrary Kubernetes/Argo status text is untrusted: output only
// fixed summaries, never raw messages (which may contain secrets or commands).
func (controller *Controller) ObserveScheduling(ctx context.Context, name, runID, sha string) (*model.ExecutionObservation, error) {
	workflow, err := controller.workflows.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return &model.ExecutionObservation{State: "not_observed", Message: "No Workflow was observed; startup or scheduling cause is unknown."}, nil
	}
	if err != nil {
		return nil, err
	}
	if workflow.Name != name || workflow.Namespace != controller.config.Namespace ||
		workflow.Annotations[runIDAnnotation] != runID || workflow.Labels["oberth.ci/sha"] != labelValue(sha) {
		return nil, errors.New("argojob: scheduling observation identity mismatch")
	}
	result := &model.ExecutionObservation{State: "observed", Phase: safePhase(string(workflow.Status.Phase)), Message: "Workflow observed; no step execution progress has been recorded."}
	if workflow.Status.Message != "" {
		result.Message = safeWorkflowMessage(workflow.Status.Message)
	}
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
	pods, err := controller.kube.CoreV1().Pods(controller.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "workflows.argoproj.io/workflow=" + name, Limit: maxSchedulingItems,
	})
	if err != nil {
		result.Pods = []model.SchedulingReason{{Phase: "Unknown", Reason: "Unavailable", Message: "Pod observation is unavailable."}}
		return result, nil
	}
	result.Truncated = result.Truncated || pods.Continue != "" || len(pods.Items) > maxSchedulingItems
	for _, pod := range pods.Items[:min(len(pods.Items), maxSchedulingItems)] {
		owner := metav1.GetControllerOf(&pod)
		if owner == nil || owner.UID != workflow.UID || owner.Name != name || owner.Kind != "Workflow" || owner.APIVersion != "argoproj.io/v1alpha1" {
			continue
		}
		result.Pods = append(result.Pods, podSchedulingReason(pod))
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
	case strings.HasPrefix(message, "Waiting for ") && strings.Contains(message, " lock. Lock status: "):
		return "Waiting for a workflow synchronization lock."
	case strings.HasPrefix(message, "Pending "):
		return "Workflow node is pending."
	case message == "Pod was deleted":
		return "Workflow pod was deleted."
	default:
		return "Controller phase message is present; free-form details withheld."
	}
}

func podSchedulingReason(pod corev1.Pod) model.SchedulingReason {
	result := model.SchedulingReason{Phase: safePhase(string(pod.Status.Phase))}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse {
			result.Reason = "Unscheduled"
			result.Message = "Pod has not been scheduled."
			if condition.Reason == "Unschedulable" {
				result.Reason = "Unschedulable"
				switch {
				case strings.Contains(condition.Message, "Insufficient cpu"):
					result.Message = "Scheduler reports insufficient CPU."
				case strings.Contains(condition.Message, "Insufficient memory"):
					result.Message = "Scheduler reports insufficient memory."
				case strings.Contains(condition.Message, "unbound immediate PersistentVolumeClaims"):
					result.Message = "Scheduler reports an unbound persistent volume claim."
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
			result.Message = "A container is waiting to start; free-form details withheld."
			return result
		}
	}
	if pod.Status.Phase == corev1.PodPending {
		result.Reason = "Pending"
		result.Message = "Pod is pending; no more specific scheduling reason was observed."
	}
	return result
}

func safeWaitingReason(reason string) string {
	switch reason {
	case "ContainerCreating", "PodInitializing", "ErrImagePull", "ImagePullBackOff", "CrashLoopBackOff", "CreateContainerConfigError", "CreateContainerError", "RunContainerError", "InvalidImageName":
		return reason
	default:
		return "Waiting"
	}
}
