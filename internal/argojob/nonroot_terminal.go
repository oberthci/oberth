package argojob

import (
	"errors"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Selection trusts the API/kubelet's final incarnation status, not phase alone.
// Historical records grant no authority. The one remaining candidate still
// undergoes every existing live controller and immutable configuration check.
func selectNonrootController(pods []corev1.Pod, namespace string) (*corev1.Pod, error) {
	var selected *corev1.Pod
	seen := make(map[types.UID]bool, len(pods))
	names := make(map[string]bool, len(pods))
	observedAt := time.Now()
	for i := range pods {
		pod := &pods[i]
		if pod.Namespace != namespace || pod.UID == "" || seen[pod.UID] || pod.Name == "" || names[pod.Name] {
			return nil, errors.New("argojob: controller Pod namespace or identity is ambiguous")
		}
		seen[pod.UID] = true
		names[pod.Name] = true
		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			if !nonrootControllerHistoryIsTerminal(pod, observedAt) {
				return nil, errors.New("argojob: controller terminal history is incomplete or contradictory")
			}
			continue
		}
		if pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil {
			return nil, errors.New("argojob: controller Pod lifecycle is not unambiguously running")
		}
		if selected != nil {
			return nil, errors.New("argojob: nonroot profile requires one unambiguous controller Pod")
		}
		selected = pod
	}
	if selected == nil {
		return nil, errors.New("argojob: nonroot profile requires one unambiguous controller Pod")
	}
	return selected, nil
}

func nonrootControllerHistoryIsTerminal(pod *corev1.Pod, observedAt time.Time) bool {
	if pod.DeletionTimestamp != nil || pod.CreationTimestamp.IsZero() || pod.CreationTimestamp.After(observedAt) || pod.ResourceVersion == "" ||
		len(pod.OwnerReferences) != 1 || len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Name != "controller" || pod.Spec.Containers[0].Image == "" ||
		len(pod.Spec.InitContainers) != 0 || len(pod.Spec.EphemeralContainers) != 0 ||
		len(pod.Status.ContainerStatuses) != 1 || len(pod.Status.InitContainerStatuses) != 0 || len(pod.Status.EphemeralContainerStatuses) != 0 {
		return false
	}
	owner := pod.OwnerReferences[0]
	if owner.APIVersion != "apps/v1" || owner.Kind != "ReplicaSet" || owner.Name == "" || owner.UID == "" || owner.Controller == nil || !*owner.Controller {
		return false
	}
	conditions := make(map[corev1.PodConditionType]corev1.ConditionStatus, len(pod.Status.Conditions))
	for _, condition := range pod.Status.Conditions {
		if _, duplicate := conditions[condition.Type]; duplicate {
			return false
		}
		conditions[condition.Type] = condition.Status
	}
	if conditions[corev1.PodReady] != corev1.ConditionFalse || conditions[corev1.ContainersReady] != corev1.ConditionFalse {
		return false
	}
	status := &pod.Status.ContainerStatuses[0]
	if status.Name != "controller" || status.Ready || status.Started == nil || *status.Started || status.RestartCount < 0 || status.Image == "" || status.ImageID == "" ||
		!nonrootTerminatedIncarnation(status.State, observedAt) || status.ContainerID != status.State.Terminated.ContainerID {
		return false
	}
	current := status.State.Terminated
	if current.StartedAt.Before(&pod.CreationTimestamp) {
		return false
	}
	// Unknown/ContainerStatusUnknown can be synthesized without observing the
	// current process exit. They must never turn missing runtime evidence into
	// a terminal record that selection can ignore.
	switch current.Reason {
	case "Completed":
		if pod.Status.Phase != corev1.PodSucceeded || current.ExitCode != 0 {
			return false
		}
	case "Error", "OOMKilled":
		if pod.Status.Phase != corev1.PodFailed || current.ExitCode <= 0 {
			return false
		}
	default:
		return false
	}
	if status.RestartCount == 0 {
		return status.LastTerminationState == (corev1.ContainerState{})
	}
	// A prior restart is not active authority, but contradictory incarnation
	// history still refuses. Its reason is not used as proof of current exit.
	if !nonrootTerminatedIncarnation(status.LastTerminationState, observedAt) {
		return false
	}
	previous := status.LastTerminationState.Terminated
	return previous.ContainerID != current.ContainerID && !previous.StartedAt.Before(&pod.CreationTimestamp) && !previous.FinishedAt.After(current.StartedAt.Time)
}

func nonrootTerminatedIncarnation(state corev1.ContainerState, observedAt time.Time) bool {
	if state.Terminated == nil || state.Running != nil || state.Waiting != nil {
		return false
	}
	terminated := state.Terminated
	runtime, id, ok := strings.Cut(terminated.ContainerID, "://")
	if !ok || runtime == "" || id == "" || len(terminated.ContainerID) > 256 || strings.ContainsAny(terminated.ContainerID, " \t\r\n\x00") {
		return false
	}
	return !terminated.StartedAt.IsZero() && !terminated.FinishedAt.IsZero() && !terminated.FinishedAt.Before(&terminated.StartedAt) && !terminated.FinishedAt.After(observedAt)
}
