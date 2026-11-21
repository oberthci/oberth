package argojob

import (
	"context"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// NativeWorkflows is the generated client's create/get/patch subset.
type NativeWorkflows interface {
	Create(context.Context, *wf.Workflow, meta.CreateOptions) (*wf.Workflow, error)
	Get(context.Context, string, meta.GetOptions) (*wf.Workflow, error)
	Patch(context.Context, string, types.PatchType, []byte, meta.PatchOptions, ...string) (*wf.Workflow, error)
}

// WorkflowClient adapts a namespace-scoped native client to the execution port.
type WorkflowClient struct{ NativeWorkflows }

func (c WorkflowClient) Patch(ctx context.Context, r PatchRequest) (*wf.Workflow, error) {
	return c.NativeWorkflows.Patch(ctx, r.Name, types.JSONPatchType, r.Body, meta.PatchOptions{})
}
