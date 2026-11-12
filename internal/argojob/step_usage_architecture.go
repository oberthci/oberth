package argojob

import (
	"runtime"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// An instrumentation binary cannot narrow an explicitly different authored
// architecture to its own. Check every scope conservatively: DAG inheritance
// can apply parent placement to a leaf as well as Workflow/template defaults.
func usageArchitectureCompatible(wf *wfv1.Workflow) bool {
	compatible := usagePlacementCompatible(wf.Spec.NodeSelector, wf.Spec.Affinity)
	visit := func(t *wfv1.Template) {
		if !usagePlacementCompatible(t.NodeSelector, t.Affinity) {
			compatible = false
		}
	}
	walkTemplate(wf.Spec.TemplateDefaults, visit, 0)
	walkTemplates(wf, visit)
	return compatible
}

func usagePlacementCompatible(selectors map[string]string, affinity *corev1.Affinity) bool {
	if arch, ok := selectors[corev1.LabelArchStable]; ok && arch != runtime.GOARCH {
		return false
	}
	if affinity == nil || affinity.NodeAffinity == nil {
		return true
	}
	check := func(term corev1.NodeSelectorTerm) bool {
		for _, r := range append(append([]corev1.NodeSelectorRequirement{}, term.MatchExpressions...), term.MatchFields...) {
			if r.Key != corev1.LabelArchStable {
				continue
			}
			switch r.Operator {
			case corev1.NodeSelectorOpDoesNotExist, corev1.NodeSelectorOpGt, corev1.NodeSelectorOpLt:
				return false
			case corev1.NodeSelectorOpNotIn:
				// Naming any excluded architecture is an authored placement choice;
				// leave it untouched rather than add a narrower binary requirement.
				return false
			}
			for _, arch := range r.Values {
				if arch != runtime.GOARCH {
					return false
				}
			}
		}
		return true
	}
	node := affinity.NodeAffinity
	if node.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		for _, term := range node.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			if !check(term) {
				return false
			}
		}
	}
	for _, term := range node.PreferredDuringSchedulingIgnoredDuringExecution {
		if !check(term.Preference) {
			return false
		}
	}
	return true
}
