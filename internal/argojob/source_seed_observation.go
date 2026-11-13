package argojob

import (
	"context"
	"errors"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/oberthci/oberth/internal/model"
)

type sourceSeedIdentity struct {
	runID, sha, podName string
	uid                 types.UID
}

// Retain only identity while this exact source copy is in progress. This map
// observes work already running; it cannot authorize or postpone preparation.
func (seeder *SourceSeeder) observeCreatedSeed(pod *corev1.Pod, requests ...*Request) func() {
	if pod == nil || pod.UID == "" || len(requests) != 1 || requests[0] == nil || requests[0].RunID == "" {
		return func() {}
	}
	request := requests[0]
	identity := sourceSeedIdentity{runID: request.RunID, sha: request.SHA, podName: pod.Name, uid: pod.UID}
	seeder.observationsMu.Lock()
	if seeder.observations == nil {
		seeder.observations = map[string]sourceSeedIdentity{}
	}
	seeder.observations[request.Name] = identity
	seeder.observationsMu.Unlock()
	return func() {
		seeder.observationsMu.Lock()
		defer seeder.observationsMu.Unlock()
		if seeder.observations[request.Name] == identity {
			delete(seeder.observations, request.Name)
		}
	}
}

func (seeder *SourceSeeder) observeScheduling(ctx context.Context, name, runID, sha string) (*model.ExecutionObservation, error) {
	seeder.observationsMu.Lock()
	identity, ok := seeder.observations[name]
	seeder.observationsMu.Unlock()
	if !ok {
		// The authenticated caller selected this run's canonical Workflow name.
		// No in-memory preparation record survives a server restart.
		identity = sourceSeedIdentity{runID: runID, sha: sha, podName: sourceClaimName(name)}
	}
	if name == "" || runID == "" || sha == "" || identity.runID != runID || identity.sha != sha {
		return nil, errors.New("argojob: source observation identity mismatch")
	}
	pod, err := seeder.client.CoreV1().Pods(seeder.config.Namespace).Get(ctx, identity.podName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if !ok {
			return nil, nil
		}
		seeder.observationsMu.Lock()
		active := seeder.observations[name] == identity
		seeder.observationsMu.Unlock()
		if !active {
			return nil, nil
		}
		return &model.ExecutionObservation{State: "observed", Phase: "Pending", Reason: "SourceSeedMissing", Message: "Source seed Pod disappeared; awaiting run finalization."}, nil
	}
	if err != nil {
		return nil, err
	}
	if pod.UID == "" || (ok && pod.UID != identity.uid) || pod.Namespace != seeder.config.Namespace || pod.Name != identity.podName {
		return nil, errors.New("argojob: source seed observation UID mismatch")
	}
	if !ok && !isSourceSeedForClaim(pod, identity.podName) {
		return nil, errors.New("argojob: source seed observation identity mismatch")
	}
	result := &model.ExecutionObservation{State: "observed", Phase: "Pending", Reason: "SourceSeedPreparing", Message: "Preparing source in the source seed Pod."}
	switch pod.Status.Phase {
	case corev1.PodPending, corev1.PodUnknown:
		pending := podSchedulingReason(*pod)
		message := pending.Message
		if pending.Reason == "" {
			pending.Reason = "Unknown"
		}
		if pending.Reason != "Pending" && pending.Reason != "Unscheduled" && pending.Reason != "Unschedulable" {
			message = "pod pending: " + pending.Reason
		}
		result.Reason = "SourceSeedPending"
		result.Message = "source seed " + message
		result.Pods = []model.SchedulingReason{pending}
	case corev1.PodFailed, corev1.PodSucceeded:
		result.Reason = "SourceSeedEnded"
		result.Message = "Source seed Pod ended; awaiting run finalization."
	}
	return result, nil
}

// Deterministic lookup observes only the known server seed shape. It neither
// adopts the Pod nor grants authority to copy source or modify its claim.
func isSourceSeedForClaim(pod *corev1.Pod, claim string) bool {
	if pod.Labels["oberth.ci/component"] != "source-seed" || len(pod.OwnerReferences) != 0 || len(pod.Spec.Volumes) != 1 || len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Name != seedContainer {
		return false
	}
	volume := pod.Spec.Volumes[0]
	return volume.Name == "source" && volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == claim
}
