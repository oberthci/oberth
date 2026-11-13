package argojob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// requestWorkflowShutdown patches only the native shutdown strategy. Callers first
// verify run ownership. UID fences replacement; Stop also compares the observed
// strategy so a concurrent Terminate cannot be overwritten. Status is not fenced.
func requestWorkflowShutdown(ctx context.Context, client WorkflowClient, workflow *wfv1.Workflow, strategy wfv1.ShutdownStrategy) (*wfv1.Workflow, error) {
	if workflow.Status.Phase.Completed() || workflow.Spec.Shutdown == strategy || workflow.Spec.Shutdown == wfv1.ShutdownStrategyTerminate {
		return workflow, nil
	}
	if workflow.UID == "" {
		return nil, fmt.Errorf("argojob: cannot Stop Workflow %s without a UID", workflow.Name)
	}
	operations := []map[string]any{{"op": "test", "path": "/metadata/uid", "value": workflow.UID}}
	if strategy == wfv1.ShutdownStrategyStop {
		path, value := "/spec/shutdown", any(workflow.Spec.Shutdown)
		if workflow.Spec.Shutdown == "" {
			// JSON Patch cannot test an absent member. Comparing spec also
			// proves shutdown is absent, without fencing status updates.
			path, value = "/spec", workflow.Spec
		}
		operations = append(operations, map[string]any{"op": "test", "path": path, "value": value})
	}
	operations = append(operations, map[string]any{"op": "add", "path": "/spec/shutdown", "value": strategy})
	patch, err := json.Marshal(operations)
	if err != nil {
		return nil, err
	}
	updated, err := client.Patch(ctx, workflow.Name, types.JSONPatchType, patch, metav1.PatchOptions{})
	// The API reports a failed JSON Patch test as Invalid; the local client
	// tracker returns ErrTestFailed. Both require the caller's bounded reread.
	if apierrors.IsInvalid(err) || errors.Is(err, jsonpatch.ErrTestFailed) {
		return nil, apierrors.NewConflict(schema.GroupResource{Group: "argoproj.io", Resource: "workflows"}, workflow.Name, err)
	}
	return updated, err
}
