package argojob

import (
	"context"
	"encoding/json"
	"fmt"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// requestWorkflowStop patches only the native shutdown strategy. Callers first
// verify run ownership. UID and resourceVersion fence replacement and races.
func requestWorkflowStop(ctx context.Context, client WorkflowClient, workflow *wfv1.Workflow) (*wfv1.Workflow, error) {
	if workflow.Status.Phase.Completed() || workflow.Spec.Shutdown == wfv1.ShutdownStrategyStop {
		return workflow, nil
	}
	if workflow.UID == "" {
		return nil, fmt.Errorf("argojob: cannot Stop Workflow %s without a UID", workflow.Name)
	}
	metadata := map[string]any{"uid": workflow.UID}
	if workflow.ResourceVersion != "" {
		metadata["resourceVersion"] = workflow.ResourceVersion
	}
	patch, err := json.Marshal(map[string]any{"metadata": metadata, "spec": map[string]any{"shutdown": wfv1.ShutdownStrategyStop}})
	if err != nil {
		return nil, err
	}
	return client.Patch(ctx, workflow.Name, types.MergePatchType, patch, metav1.PatchOptions{})
}
