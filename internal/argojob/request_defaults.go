package argojob

import (
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const requestDefaultsAnnotation = "oberth.ci/request-defaults"
const legacyRequestDefaultsPolicy = "2cpu-2Gi-v1"
const requestDefaultsPolicy = "2cpu-2Gi-helpers-250m-256Mi-v2"

// Fill only missing request dimensions before server helper injection. Existing
// declarations, inherited template values, and Kubernetes' limits-only request
// semantics take precedence. Limits and explicit positive requests never change.

func applyRequestDefaultsPolicy(wf *wfv1.Workflow, omit, legacyHelpers bool) {
	delete(wf.Annotations, requestDefaultsAnnotation)
	if omit {
		return
	}
	request := func(c *corev1.Container, inherited *corev1.Container, helper bool) {
		if c == nil {
			return
		}
		if c.Resources.Requests == nil {
			c.Resources.Requests = corev1.ResourceList{}
		}
		cpu, memory := "2", "2Gi"
		if helper && !legacyHelpers {
			cpu, memory = "250m", "256Mi"
		}
		for _, value := range []struct {
			name     corev1.ResourceName
			fallback string
		}{{corev1.ResourceCPU, cpu}, {corev1.ResourceMemory, memory}} {
			if q, ok := c.Resources.Requests[value.name]; ok && q.Sign() != 0 {
				continue // Negative declarations remain visible to validation.
			}
			q := resource.MustParse(value.fallback)
			if limit, ok := c.Resources.Limits[value.name]; ok && limit.Sign() > 0 {
				q = limit.DeepCopy()
			}
			if inherited != nil {
				if v, ok := inherited.Resources.Requests[value.name]; ok && v.Sign() > 0 {
					q = v.DeepCopy()
				} else if _, ownLimit := c.Resources.Limits[value.name]; !ownLimit {
					if v, ok := inherited.Resources.Limits[value.name]; ok && v.Sign() > 0 {
						q = v.DeepCopy()
					}
				}
			}
			if limit, ok := c.Resources.Limits[value.name]; ok && limit.Sign() > 0 && q.Cmp(limit) > 0 {
				q = limit.DeepCopy()
			}
			c.Resources.Requests[value.name] = q
		}
	}
	defaults := wf.Spec.TemplateDefaults
	userDefault := func(containers []wfv1.UserContainer, name string) *corev1.Container {
		for i := range containers {
			if containers[i].Name == name {
				return &containers[i].Container
			}
		}
		return nil
	}
	visit := func(t *wfv1.Template) {
		var containerDefault, scriptDefault *corev1.Container
		if defaults != nil {
			containerDefault = defaults.Container
			if defaults.Script != nil {
				scriptDefault = &defaults.Script.Container
			}
		}
		request(t.Container, containerDefault, false)
		if t.Script != nil {
			request(&t.Script.Container, scriptDefault, false)
		}
		if t.ContainerSet != nil {
			for i := range t.ContainerSet.Containers {
				var inherited *corev1.Container
				if defaults != nil && defaults.ContainerSet != nil {
					for j := range defaults.ContainerSet.Containers {
						if defaults.ContainerSet.Containers[j].Name == t.ContainerSet.Containers[i].Name {
							inherited = &defaults.ContainerSet.Containers[j].Container
						}
					}
				}
				request(&t.ContainerSet.Containers[i].Container, inherited, false)
			}
		}
		for i := range t.InitContainers {
			var inherited *corev1.Container
			if defaults != nil {
				inherited = userDefault(defaults.InitContainers, t.InitContainers[i].Name)
			}
			request(&t.InitContainers[i].Container, inherited, true)
		}
		for i := range t.Sidecars {
			var inherited *corev1.Container
			if defaults != nil {
				inherited = userDefault(defaults.Sidecars, t.Sidecars[i].Name)
			}
			request(&t.Sidecars[i].Container, inherited, true)
		}
	}
	walkTemplate(defaults, visit, 0)
	walkTemplates(wf, visit)
	if wf.Annotations == nil {
		wf.Annotations = map[string]string{}
	}
	wf.Annotations[requestDefaultsAnnotation] = requestDefaultsPolicy
	if legacyHelpers {
		wf.Annotations[requestDefaultsAnnotation] = legacyRequestDefaultsPolicy
	}
}
