package argojob

import (
	"context"
	"errors"
	"fmt"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/workflow/packer"
)

var errNodeStatusOffloaded = errors.New("argojob: step rows are unavailable because Argo node status is offloaded")

// Native compression is decoded with Argo's reader, on a copy of shared cache
// objects. Missing/offloaded status never becomes an invented empty snapshot.
func nativeNodeWorkflow(ctx context.Context, workflow *wfv1.Workflow) (*wfv1.Workflow, error) {
	if workflow == nil {
		return nil, nil
	}
	if len(workflow.Status.Nodes) == 0 && workflow.Status.OffloadNodeStatusVersion != "" {
		return nil, errNodeStatusOffloaded
	}
	if workflow.Status.CompressedNodes == "" {
		return workflow, nil
	}
	expanded := workflow.DeepCopy()
	if err := packer.DecompressWorkflow(ctx, expanded); err != nil {
		return nil, fmt.Errorf("decode Argo node status: %w", err)
	}
	return expanded, nil
}

// observedNodeWorkflow preserves top-level phase observation when Argo stores
// nodes elsewhere. Row and replay readers still require nativeNodeWorkflow.
func observedNodeWorkflow(ctx context.Context, workflow *wfv1.Workflow) (*wfv1.Workflow, error) {
	expanded, err := nativeNodeWorkflow(ctx, workflow)
	if errors.Is(err, errNodeStatusOffloaded) {
		return workflow, nil
	}
	return expanded, err
}
