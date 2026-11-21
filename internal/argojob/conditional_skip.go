package argojob

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	core "k8s.io/api/core/v1"
	"slices"
)

// Skipped/Skipped means the step did not run in this native lifecycle.
// Native retry or resubmission by direct Workflow authority is outside the boundary;
// the product never performs either operation.
func neverRunSkip(n wf.NodeStatus, pods []*core.Pod) bool {
	return n.Type == wf.NodeTypeSkipped && n.Phase == wf.NodeSkipped &&
		!slices.ContainsFunc(pods, func(p *core.Pod) bool {
			return p != nil && p.Annotations["workflows.argoproj.io/node-id"] == n.ID
		})
}
