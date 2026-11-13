package argojob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func sourceWorkflowOwner(claim *corev1.PersistentVolumeClaim, workflowName string) (metav1.OwnerReference, error) {
	for _, owner := range claim.OwnerReferences {
		if owner.APIVersion == "argoproj.io/v1alpha1" && owner.Kind == "Workflow" && owner.Name == workflowName && owner.UID != "" {
			// Lifetime ownership only: never claim Argo controller ownership or
			// block its deletion (which would require workflows/finalizers rights).
			return metav1.OwnerReference{APIVersion: owner.APIVersion, Kind: owner.Kind, Name: owner.Name, UID: owner.UID}, nil
		}
	}
	return metav1.OwnerReference{}, errors.New("argojob: artifact source claim lacks Workflow ownership")
}

// Usually fill already deleted the seed. Repair a surviving Pod after the
// Workflow create. An ownerless seed left by a crash or patch failure is
// removed by the existing orphan sweep after its grace period.
func (seeder *SourceSeeder) adoptSeedPod(ctx context.Context, claim *corev1.PersistentVolumeClaim, workflowName, workflowUID string) error {
	pods := seeder.client.CoreV1().Pods(seeder.config.Namespace)
	pod, err := pods.Get(ctx, claim.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	owner := metav1.OwnerReference{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: workflowName, UID: types.UID(workflowUID)}
	if len(pod.OwnerReferences) == 1 && pod.OwnerReferences[0] == owner {
		return nil
	}
	if !isSourceSeedForClaim(pod, claim.Name) || pod.UID == "" {
		return errors.New("argojob: surviving source seed identity differs")
	}
	patch, err := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": pod.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": pod.ResourceVersion},
		{"op": "add", "path": "/metadata/ownerReferences", "value": []metav1.OwnerReference{owner}},
	})
	if err != nil {
		return err
	}
	if _, err := pods.Patch(ctx, pod.Name, types.JSONPatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("argojob: bind source seed Pod to its Workflow: %w", err)
	}
	return nil
}

// Only the canonical ownerless seed is eligible. Observation shape checks and
// UID-preconditioned deletion leave replacements and Workflow-owned Pods alone.
func (seeder *SourceSeeder) sweepUnownedSeed(ctx context.Context, claimName string) bool {
	pods := seeder.client.CoreV1().Pods(seeder.config.Namespace)
	pod, err := pods.Get(ctx, claimName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true
	}
	if err != nil || !isSourceSeedForClaim(pod, claimName) || pod.UID == "" || time.Since(pod.CreationTimestamp.Time) < seeder.orphanGrace() {
		return false
	}
	options := *metav1.NewDeleteOptions(0)
	options.Preconditions = &metav1.Preconditions{UID: &pod.UID}
	err = pods.Delete(ctx, pod.Name, options)
	return err == nil || apierrors.IsNotFound(err)
}
