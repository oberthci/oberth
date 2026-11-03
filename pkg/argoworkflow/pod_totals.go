package argoworkflow

import (
	"maps"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

// PodRequests is a request envelope without admission helpers or headroom.
// A static declared total includes sequential work; a peak includes only work
// that can overlap. Each peak dimension is an independent upper bound.
type PodRequests struct {
	ExtendedResources map[string]int64 `json:"extended_resources,omitempty"`
	CPUMilli          int64            `json:"cpu_milli"`
	MemoryBytes       int64            `json:"memory_bytes"`
	Pods              int64            `json:"pods"`
}

func (r *PodRequests) Clone() *PodRequests {
	if r == nil {
		return nil
	}
	c := *r
	c.ExtendedResources = maps.Clone(r.ExtendedResources)
	return &c
}

func validPodTotal(d *PodRequests) bool {
	return d == nil || d.CPUMilli >= 0 && d.CPUMilli <= 1_000_000_000 && d.MemoryBytes >= 0 && d.MemoryBytes <= 1<<60 && d.Pods > 0 && d.Pods <= 256 && ValidateExtendedResourceLimits(d.ExtendedResources) == nil
}

// declaredPodTotal reuses the bounded expansion without concurrency caps. A
// dynamic fan-out or graph beyond the analysis bound has no known static total;
// this optional diagnostic never changes a peak or enables size fallback.
func declaredPodTotal(wf *wfv1.Workflow, resourcePods bool) *PodRequests {
	g := podGraph{uncapped: true, resourcePods: resourcePods, templates: map[string]*wfv1.Template{}, defaults: wf.Spec.TemplateDefaults, stack: map[*wfv1.Template]bool{}}
	for i := range wf.Spec.Templates {
		t := &wf.Spec.Templates[i]
		g.templates[t.Name] = t
	}
	if _, _, err := g.expand(g.templates[wf.Spec.Entrypoint], 0); err != nil {
		return nil
	}
	total := &PodRequests{}
	for _, d := range g.leaves {
		total.CPUMilli += d.CPUMilli
		total.MemoryBytes += d.MemoryBytes
		total.Pods += d.Pods
		total.ExtendedResources = sumExtended(total.ExtendedResources, d.ExtendedResources)
	}
	if !validPodTotal(total) {
		return nil
	}
	return total
}
