package vmrunner

import (
	"errors"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// KVMTerminationBinding must come from immutable server admission and observed
// API ownership/process identity, not workflow labels, logs or guest output.
// The future runtime must separately verify the server-owned pod command, args,
// effective template and isolation (including sidecars and shared mounts). An
// image digest alone does not prove those properties. That runtime is not yet
// implemented by this foundation.
type KVMTerminationBinding struct {
	Namespace       string
	WorkflowName    string
	WorkflowUID     string
	PodName         string
	PodUID          string
	ContainerID     string
	HarnessImageRef string
	AdmittedAt      time.Time
	Deadline        time.Time
}

// VerifyKVMTermination checks one current kubelet termination against a prior
// server binding. It supplies only the process half of future receipt validation:
// a clean QEMU shutdown does not prove guest tests passed. No scheduler or
// publication path treats success here as authority; a protected harness result,
// verified inventory, candidate/artifact binding and journal are still required.
func VerifyKVMTermination(binding KVMTerminationBinding, pod *corev1.Pod) error {
	if binding.Namespace == "" || binding.WorkflowName == "" || binding.WorkflowUID == "" || binding.PodName == "" || binding.PodUID == "" || binding.ContainerID == "" ||
		binding.AdmittedAt.IsZero() || !binding.Deadline.After(binding.AdmittedAt) || binding.Deadline.Sub(binding.AdmittedAt) > MaxDeadline || len(validateGuestImageRef(binding.HarnessImageRef)) != 0 {
		return errors.New("trusted termination requires complete immutable admission and process binding")
	}
	if pod == nil || pod.Namespace != binding.Namespace || pod.Name != binding.PodName || string(pod.UID) != binding.PodUID || pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodSucceeded {
		return errors.New("trusted termination pod identity or terminal phase mismatch")
	}
	if len(pod.OwnerReferences) != 1 {
		return errors.New("trusted termination requires its exact workflow controller owner")
	}
	owner := pod.OwnerReferences[0]
	if owner.APIVersion != "argoproj.io/v1alpha1" || owner.Kind != "Workflow" || owner.Name != binding.WorkflowName || string(owner.UID) != binding.WorkflowUID || owner.Controller == nil || !*owner.Controller {
		return errors.New("trusted termination workflow owner mismatch")
	}
	var main *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "main" {
			if main != nil {
				return errors.New("trusted pod has duplicate main containers")
			}
			main = &pod.Spec.Containers[i]
		}
	}
	if main == nil || main.Image != binding.HarnessImageRef {
		return errors.New("trusted termination harness reference mismatch")
	}
	var status *corev1.ContainerStatus
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == "main" {
			if status != nil {
				return errors.New("trusted pod has duplicate main statuses")
			}
			status = &pod.Status.ContainerStatuses[i]
		}
	}
	if status == nil || status.ContainerID != binding.ContainerID || status.RestartCount != 0 || status.LastTerminationState.Terminated != nil || status.State.Running != nil || status.State.Waiting != nil || status.State.Terminated == nil {
		return errors.New("trusted termination requires one bound container attempt with no restarts")
	}
	expectedDigest := binding.HarnessImageRef[strings.LastIndex(binding.HarnessImageRef, "@")+1:]
	if runtimeImageDigest(status.ImageID) != expectedDigest {
		return errors.New("trusted termination kubelet image digest mismatch")
	}
	terminated := status.State.Terminated
	if terminated.ContainerID != binding.ContainerID || terminated.ExitCode != 0 || terminated.Signal != 0 {
		return errors.New("trusted termination container identity, exit code or signal mismatch")
	}
	started, finished := terminated.StartedAt.Time, terminated.FinishedAt.Time
	if started.IsZero() || finished.IsZero() || started.Before(binding.AdmittedAt) || finished.Before(started) || finished.After(binding.Deadline) {
		return errors.New("trusted termination timestamps are missing or outside the admitted deadline")
	}
	return nil
}

// Only canonical runtime encodings are accepted; a digest substring is not an
// identity. Image config digests that differ from the admitted manifest digest
// fail closed and require separate runtime qualification before activation.
func runtimeImageDigest(imageID string) string {
	if ociDigestPattern.MatchString(imageID) {
		return imageID
	}
	if strings.HasPrefix(imageID, "docker://") {
		digest := strings.TrimPrefix(imageID, "docker://")
		if ociDigestPattern.MatchString(digest) {
			return digest
		}
		return ""
	}
	ref := strings.TrimPrefix(imageID, "docker-pullable://")
	if strings.Contains(ref, "://") {
		return ""
	}
	if len(validateGuestImageRef(ref)) != 0 {
		return ""
	}
	return ref[strings.LastIndex(ref, "@")+1:]
}
