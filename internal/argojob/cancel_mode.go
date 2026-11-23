package argojob

import (
	"context"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/workflow/packer"
)

// Choose from the live native state, since stored progress can lag the start.
func cancellationMode(ctx context.Context, w *wf.Workflow, requested wf.ShutdownStrategy) (wf.ShutdownStrategy, error) {
	if requested == wf.ShutdownStrategyTerminate || w.Status.OffloadNodeStatusVersion != "" {
		return requested, nil
	}
	if err := packer.DecompressWorkflow(ctx, w); err != nil {
		return "", err
	}
	for _, n := range w.Status.Nodes {
		if executable(n) && (n.Phase == wf.NodeRunning || n.Phase == wf.NodeSucceeded || terminalNode(n)) {
			return requested, nil
		}
	}
	return wf.ShutdownStrategyTerminate, nil
}
