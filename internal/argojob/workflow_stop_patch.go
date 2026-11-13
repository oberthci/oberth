package argojob

import (
	"context"
	"encoding/json"
	"fmt"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// requestWorkflowShutdown patches only the native shutdown strategy. Callers first
// verify run ownership. UID fences replacement; status churn does not reject a shutdown.
func requestWorkflowShutdown(ctx context.Context, client WorkflowClient, workflow *wfv1.Workflow, strategy wfv1.ShutdownStrategy) (*wfv1.Workflow, error) {
	if workflow.Status.Phase.Completed() || workflow.Spec.Shutdown == strategy || workflow.Spec.Shutdown == wfv1.ShutdownStrategyTerminate {
		return workflow, nil
	}
	if workflow.UID == "" {
		return nil, fmt.Errorf("argojob: cannot Stop Workflow %s without a UID", workflow.Name)
	}
	metadata := map[string]any{"uid": workflow.UID}
	patch, err := json.Marshal(map[string]any{"metadata": metadata, "spec": map[string]any{"shutdown": strategy}})
	if err != nil {
		return nil, err
	}
	return client.Patch(ctx, workflow.Name, types.MergePatchType, patch, metav1.PatchOptions{})
}
