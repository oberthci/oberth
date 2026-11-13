package argoworkflow

import (
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

type podContainer struct {
	container, defaults *corev1.Container
}

// Template defaults merge helper lists by name. Check inherited limits on the
// effective helper, and count that helper once rather than appending duplicates.
func podHelpers(authored, inherited []wfv1.UserContainer) []podContainer {
	defaults := map[string]*corev1.Container{}
	for i := range inherited {
		defaults[inherited[i].Name] = &inherited[i].Container
	}
	shadowed := map[string]bool{}
	var out []podContainer
	for i := range authored {
		c := &authored[i].Container
		out = append(out, podContainer{c, defaults[c.Name]})
		shadowed[c.Name] = true
	}
	for i := range inherited {
		c := &inherited[i].Container
		if !shadowed[c.Name] {
			out = append(out, podContainer{container: c})
		}
	}
	return out
}

// EachTemplateHelper visits the sidecars or init containers Argo runs for a
// template after spec.templateDefaults merge by name: each authored entry with
// the default of the same name (or nil), then each default-only entry with nil.
// Security and helper-storage checks share this native merge behavior.
func EachTemplateHelper(authored, inherited []wfv1.UserContainer, visit func(container, defaults *corev1.Container)) {
	for _, c := range podHelpers(authored, inherited) {
		visit(c.container, c.defaults)
	}
}
