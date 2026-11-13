package argojob

import (
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// walkTemplates visits every template of the workflow, inline templates
// included, with the same depth bound every other identity-bearing walk uses.
func walkTemplates(workflow *wfv1.Workflow, visit func(*wfv1.Template)) {
	for index := range workflow.Spec.Templates {
		walkTemplate(&workflow.Spec.Templates[index], visit, 0)
	}
}

func walkTemplate(template *wfv1.Template, visit func(*wfv1.Template), depth int) {
	if template == nil || depth > argoworkflow.MaxIdentityWalkDepth {
		return
	}
	visit(template)
	for group := range template.Steps {
		for step := range template.Steps[group].Steps {
			walkTemplate(template.Steps[group].Steps[step].Inline, visit, depth+1)
		}
	}
	if template.DAG != nil {
		for task := range template.DAG.Tasks {
			walkTemplate(template.DAG.Tasks[task].Inline, visit, depth+1)
		}
	}
}
