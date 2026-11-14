package argojob

import (
	"context"
	"encoding/json"
	"errors"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func applyWorkflowStopTestPatch(workflow *wfv1.Workflow, kind types.PatchType, data []byte) (*wfv1.Workflow, error) {
	if workflow == nil {
		return nil, apierrors.NewNotFound(workflowTestResource, "missing")
	}
	var patch struct {
		Metadata struct {
			UID             types.UID `json:"uid"`
			ResourceVersion string    `json:"resourceVersion"`
		} `json:"metadata"`
		Spec struct {
			Shutdown wfv1.ShutdownStrategy `json:"shutdown"`
		} `json:"spec"`
	}
	if kind != types.MergePatchType {
		return nil, errors.New("expected MergePatch")
	}
	if err := json.Unmarshal(data, &patch); err != nil {
		return nil, err
	}
	if patch.Metadata.UID != workflow.UID || patch.Metadata.ResourceVersion != "" && patch.Metadata.ResourceVersion != workflow.ResourceVersion {
		return nil, apierrors.NewConflict(workflowTestResource, workflow.Name, errors.New("UID/resourceVersion changed"))
	}
	if patch.Spec.Shutdown != wfv1.ShutdownStrategyStop && patch.Spec.Shutdown != wfv1.ShutdownStrategyTerminate {
		return nil, errors.New("expected native shutdown")
	}
	next := workflow.DeepCopy()
	next.Spec.Shutdown = patch.Spec.Shutdown
	return next, nil
}
func (client *staticWorkflowClient) Patch(_ context.Context, _ string, kind types.PatchType, data []byte, _ metav1.PatchOptions, _ ...string) (*wfv1.Workflow, error) {
	next, err := applyWorkflowStopTestPatch(client.workflow, kind, data)
	if err == nil {
		client.workflow = next
	}
	return next, err
}
func (client *createRetryWorkflowClient) Patch(_ context.Context, _ string, kind types.PatchType, data []byte, _ metav1.PatchOptions, _ ...string) (*wfv1.Workflow, error) {
	next, err := applyWorkflowStopTestPatch(client.stored, kind, data)
	if err == nil {
		client.stored = next
	}
	return next, err
}
func (client *persistedWorkflowClient) Patch(_ context.Context, _ string, kind types.PatchType, data []byte, _ metav1.PatchOptions, _ ...string) (*wfv1.Workflow, error) {
	next, err := applyWorkflowStopTestPatch(client.existing, kind, data)
	if err == nil {
		client.existing = next
	}
	return next, err
}
func (unusedWorkflows) Patch(context.Context, string, types.PatchType, []byte, metav1.PatchOptions, ...string) (*wfv1.Workflow, error) {
	panic("Patch must not reach the Workflow client without a source volume")
}
