package argojob

import (
	"fmt"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"strings"
)

// Inline nodes have no template name. Their boundary and invocation identify
// the sealed inline declaration; a missing or cyclic boundary cannot resolve one.
func nodeTemplate(w *wf.Workflow, n wf.NodeStatus, depth int) *wf.Template {
	scope, resource := n.GetTemplateScope()
	if n.TemplateRef != nil || n.TemplateName != "" && scope != wf.ResourceScopeLocal {
		return w.GetStoredTemplate(scope, resource, &n)
	}
	if n.TemplateName != "" {
		return w.GetTemplateByName(n.TemplateName)
	}
	if depth == 0 {
		return nil
	}
	parent, ok := w.Status.Nodes[n.BoundaryID]
	if !ok {
		return nil
	}
	t := nodeTemplate(w, parent, depth-1)
	if t == nil {
		return nil
	}
	suffix, ok := strings.CutPrefix(n.Name, parent.Name)
	if !ok {
		return nil
	}
	return inlineNodeTemplate(t, suffix)
}
func inlineNodeTemplate(t *wf.Template, suffix string) *wf.Template {
	for group, steps := range t.Steps {
		for _, step := range steps.Steps {
			if step.Inline != nil && inlineNodeCall(suffix, fmt.Sprintf("[%d].%s", group, step.Name)) {
				return step.Inline
			}
		}
	}
	if t.DAG != nil {
		for _, task := range t.DAG.Tasks {
			if task.Inline != nil && inlineNodeCall(suffix, "."+task.Name) {
				return task.Inline
			}
		}
	}
	return nil
}
func inlineNodeCall(suffix, call string) bool {
	return suffix == call || strings.HasPrefix(suffix, call+"(")
}
