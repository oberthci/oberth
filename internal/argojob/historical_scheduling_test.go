package argojob

import (
	"encoding/json"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

// These immutable controller captures exercise security isolation under the
// old submission policy. Reconstruct only their retired scheduling inputs;
// current submissions are covered independently by native field preservation
// tests. No captured bytes or provenance hashes are rewritten.
func historicalSchedulingInput(t *testing.T, wf *wfv1.Workflow, release bool) {
	t.Helper()
	wf.Spec.PodPriorityClassName = "oberth-argo-oberth-pipeline"
	if release {
		wf.Spec.Priority = ptr.To(int32(1))
	}
	var patch map[string]any
	if wf.Spec.PodSpecPatch != "" {
		if err := json.Unmarshal([]byte(wf.Spec.PodSpecPatch), &patch); err != nil {
			t.Fatal(err)
		}
	}
	if patch == nil {
		patch = map[string]any{}
	}
	patch["preemptionPolicy"] = "Never"
	encoded, err := json.Marshal(patch)
	if err != nil {
		t.Fatal(err)
	}
	wf.Spec.PodSpecPatch = string(encoded)
	if wf.Spec.ActiveDeadlineSeconds != nil && *wf.Spec.ActiveDeadlineSeconds > 7200 {
		wf.Spec.ActiveDeadlineSeconds = ptr.To(int64(7200))
	}
	fill := func(c *corev1.Container, helper bool) {
		if c == nil {
			return
		}
		if c.Resources.Requests == nil {
			c.Resources.Requests = corev1.ResourceList{}
		}
		defaults := map[corev1.ResourceName]string{corev1.ResourceCPU: "2", corev1.ResourceMemory: "2Gi"}
		if helper {
			defaults[corev1.ResourceCPU], defaults[corev1.ResourceMemory] = "250m", "256Mi"
		}
		for name, value := range defaults {
			if _, ok := c.Resources.Requests[name]; !ok {
				q := resource.MustParse(value)
				if limit, ok := c.Resources.Limits[name]; ok && limit.Sign() > 0 {
					q = limit
				}
				c.Resources.Requests[name] = q
			}
		}
	}
	walkTemplates(wf, func(template *wfv1.Template) {
		fill(template.Container, false)
		if template.Script != nil {
			fill(&template.Script.Container, false)
		}
		if template.ContainerSet != nil {
			for i := range template.ContainerSet.Containers {
				fill(&template.ContainerSet.Containers[i].Container, false)
			}
		}
		for i := range template.InitContainers {
			fill(&template.InitContainers[i].Container, true)
		}
		for i := range template.Sidecars {
			fill(&template.Sidecars[i].Container, true)
		}
	})
	wf.Annotations["oberth.ci/request-defaults"] = "2cpu-2Gi-helpers-250m-256Mi-v2"
	identity, err := specIdentity(wf)
	if err != nil {
		t.Fatal(err)
	}
	wf.Annotations[identityAnnotation] = identity
}

func assertHistoricalPipelinePriority(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	if pod.Spec.PriorityClassName != "oberth-argo-oberth-pipeline" || pod.Spec.PreemptionPolicy == nil || *pod.Spec.PreemptionPolicy != corev1.PreemptNever {
		t.Fatal("historical Pod lost its captured non-preempting helper class")
	}
}
