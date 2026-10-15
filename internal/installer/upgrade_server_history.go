package installer

import (
	"encoding/hex"
	"errors"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// selectUpgradeServer trusts API/kubelet final-incarnation status only to
// exclude inert history. The selected live Pod still passes every existing
// Deployment/ReplicaSet/template/readiness and fixed-version check.
func selectUpgradeServer(pods []corev1.Pod, namespace string, observedAt time.Time) (*corev1.Pod, error) {
	fail := errors.New("oberth server Pod lifecycle or identity ambiguous")
	var selected *corev1.Pod
	names := map[string]bool{}
	uids := map[types.UID]bool{}
	for i := range pods {
		p := &pods[i]
		if p.Namespace != namespace || p.Name == "" || p.UID == "" || names[p.Name] || uids[p.UID] {
			return nil, fail
		}
		names[p.Name] = true
		uids[p.UID] = true
		if p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded {
			if !upgradeServerHistoryTerminal(p, observedAt) {
				return nil, fail
			}
			continue
		}
		if p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil || selected != nil {
			return nil, fail
		}
		selected = p
	}
	if selected == nil {
		return nil, fail
	}
	return selected, nil
}

func upgradeServerHistoryTerminal(p *corev1.Pod, now time.Time) bool {
	if p.DeletionTimestamp != nil || p.ResourceVersion == "" || p.CreationTimestamp.IsZero() || p.CreationTimestamp.After(now) || len(p.OwnerReferences) != 1 || len(p.Spec.Containers) != 1 || len(p.Spec.InitContainers) != 1 || len(p.Spec.EphemeralContainers) != 0 || len(p.Status.ContainerStatuses) != 1 || len(p.Status.InitContainerStatuses) != 1 || len(p.Status.EphemeralContainerStatuses) != 0 {
		return false
	}
	owner := p.OwnerReferences[0]
	if owner.APIVersion != "apps/v1" || owner.Kind != "ReplicaSet" || owner.Name == "" || owner.UID == "" || owner.Controller == nil || !*owner.Controller {
		return false
	}
	ordinary, init := p.Spec.Containers[0], p.Spec.InitContainers[0]
	if ordinary.Name != "oberth" || init.Name != "prepare-caches" || ordinary.RestartPolicy != nil || init.RestartPolicy != nil || ordinary.Image != init.Image || !upgradeServerImage(ordinary.Image) {
		return false
	}
	conditions := map[corev1.PodConditionType]corev1.ConditionStatus{}
	for _, c := range p.Status.Conditions {
		if _, ok := conditions[c.Type]; ok {
			return false
		}
		conditions[c.Type] = c.Status
	}
	if conditions[corev1.PodReady] != corev1.ConditionFalse || conditions[corev1.ContainersReady] != corev1.ConditionFalse || conditions[corev1.PodInitialized] != corev1.ConditionTrue {
		return false
	}
	current, prepared := p.Status.ContainerStatuses[0], p.Status.InitContainerStatuses[0]
	if current.Name != ordinary.Name || prepared.Name != init.Name || current.Ready || !prepared.Ready || !upgradeServerTerminatedStatus(current, p.CreationTimestamp.Time, now) || !upgradeServerTerminatedStatus(prepared, p.CreationTimestamp.Time, now) {
		return false
	}
	mainExit, initExit := current.State.Terminated, prepared.State.Terminated
	// A normal one-shot completed init remains Ready=true in kubelet status.
	// It is not a running sidecar: RestartAlways, Started=true and any live state
	// refuse above. Its successful completion must precede the ordinary process.
	if initExit.ContainerID == mainExit.ContainerID || initExit.Reason != "Completed" || initExit.ExitCode != 0 || initExit.Signal != 0 || initExit.FinishedAt.After(mainExit.StartedAt.Time) {
		return false
	}
	switch mainExit.Reason {
	case "Completed":
		return p.Status.Phase == corev1.PodSucceeded && mainExit.ExitCode == 0 && mainExit.Signal == 0
	case "Error", "OOMKilled":
		return p.Status.Phase == corev1.PodFailed && mainExit.ExitCode > 0
	default:
		return false
	}
}

func upgradeServerImage(image string) bool {
	const prefix = "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth@sha256:"
	return strings.HasPrefix(image, prefix) && upgradeServerHex(strings.TrimPrefix(image, prefix))
}
func upgradeServerHex(value string) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == 32 && hex.EncodeToString(raw) == value && value != strings.Repeat("0", 64)
}
func upgradeServerIncarnation(s corev1.ContainerState, now time.Time) bool {
	if s.Terminated == nil || s.Running != nil || s.Waiting != nil {
		return false
	}
	t := s.Terminated
	return strings.HasPrefix(t.ContainerID, "containerd://") && upgradeServerHex(strings.TrimPrefix(t.ContainerID, "containerd://")) && !t.StartedAt.IsZero() && !t.FinishedAt.IsZero() && !t.FinishedAt.Before(&t.StartedAt) && !t.FinishedAt.After(now)
}
func upgradeServerTerminatedStatus(s corev1.ContainerStatus, created, now time.Time) bool {
	if s.Started == nil || *s.Started || s.RestartCount < 0 || s.Image == "" || s.ImageID == "" || !upgradeServerIncarnation(s.State, now) || s.ContainerID != s.State.Terminated.ContainerID || s.State.Terminated.StartedAt.Time.Before(created) {
		return false
	}
	if s.RestartCount == 0 {
		return s.LastTerminationState == (corev1.ContainerState{})
	}
	if !upgradeServerIncarnation(s.LastTerminationState, now) {
		return false
	}
	previous, current := s.LastTerminationState.Terminated, s.State.Terminated
	return previous.ContainerID != current.ContainerID && !previous.StartedAt.Time.Before(created) && !previous.FinishedAt.After(current.StartedAt.Time)
}
