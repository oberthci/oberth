package argojob

import (
	"context"
	"encoding/json"
	"errors"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func applyWorkflowStopTestPatch(workflow *wfv1.Workflow, kind types.PatchType, data []byte) (*wfv1.Workflow, error) {
	if workflow == nil {
		return nil, apierrors.NewNotFound(workflowTestResource, "missing")
	}
	if kind != types.JSONPatchType {
		return nil, errors.New("expected JSONPatch")
	}
	patch, err := jsonpatch.DecodePatch(data)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(workflow)
	if err != nil {
		return nil, err
	}
	updated, err := patch.Apply(body)
	if err != nil {
		return nil, err
	}
	next := &wfv1.Workflow{}
	if err := json.Unmarshal(updated, next); err != nil {
		return nil, err
	}

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
