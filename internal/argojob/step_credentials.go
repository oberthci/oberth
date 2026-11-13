package argojob

import (
	"sort"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// StepCredentialEvidence snapshots the compiled admission's template/grant
// facts before any Workflow is created. Never call it on a watched Workflow:
// later mutable YAML cannot prove that a previous step had no credentials.
func StepCredentialEvidence(workflow *wfv1.Workflow, request Request) []model.StepCredentialEvidence {
	complete := replayTemplateGraphComplete(workflow)
	result := make([]model.StepCredentialEvidence, 0, len(workflow.Spec.Templates))
	for _, template := range workflow.Spec.Templates {
		grants := []string{}
		known := template.Container != nil || template.Script != nil || template.ContainerSet != nil
		if template.Resource != nil && request.Capability != nil {
			known = true
			grants = append(grants, "capability:"+request.Capability.Namespace+"/"+request.Capability.ServiceAccount)
		}
		for path, steps := range request.StepGrants {
			if steps[template.Name] || steps["*"] {
				grants = append(grants, "secret:"+path)
			}
		}
		if role := template.Metadata.Annotations[argoworkflow.ReleaseWIFRoleAnnotation]; role != "" {
			grants = append(grants, "release-wif:"+role)
		}
		volumesByScope := [][]corev1.Volume{workflow.Spec.Volumes, template.Volumes}
		if workflow.Spec.TemplateDefaults != nil {
			volumesByScope = append(volumesByScope, workflow.Spec.TemplateDefaults.Volumes)
		}
		for _, volumes := range volumesByScope {
			for _, volume := range volumes {
				if volume.Projected == nil {
					continue
				}
				for _, source := range volume.Projected.Sources {
					if source.ServiceAccountToken != nil {
						grants = append(grants, "service-account-token")
					}
				}
			}
		}
		sort.Strings(grants)
		result = append(result, model.StepCredentialEvidence{Known: known, Complete: complete, Template: template.Name, Credentialed: len(grants) > 0 || argoworkflow.UsesCredentials(&template), Grants: grants})
	}
	return result
}

// replayTemplateGraphComplete refuses replay authority if a template walk
// would cut a back edge or cannot resolve an invocation. Admission still
// accepts the vanilla graph; display and execution use native node IDs.
func replayTemplateGraphComplete(workflow *wfv1.Workflow) bool {
	plan, err := argoworkflow.PlanSteps(workflow)
	if err != nil || plan.BackEdgeCut {
		return false
	}
	templates := make(map[string]*wfv1.Template, len(workflow.Spec.Templates))
	for i := range workflow.Spec.Templates {
		t := &workflow.Spec.Templates[i]
		templates[t.Name] = t
	}
	state := map[*wfv1.Template]uint8{}
	var visit func(*wfv1.Template, int) bool
	visit = func(t *wfv1.Template, depth int) bool {
		if t == nil || depth > argoworkflow.MaxIdentityWalkDepth || state[t] == 1 {
			return false
		}
		if state[t] == 2 {
			return true
		}
		state[t] = 1
		check := func(name string, inline *wfv1.Template, ref *wfv1.TemplateRef) bool {
			if ref != nil {
				return false
			}
			if inline != nil {
				return visit(inline, depth+1)
			}
			return visit(templates[name], depth+1)
		}
		if t.DAG != nil {
			for _, task := range t.DAG.Tasks {
				if !check(task.Template, task.Inline, task.TemplateRef) {
					return false
				}
			}
		}
		for _, group := range t.Steps {
			for _, step := range group.Steps {
				if !check(step.Template, step.Inline, step.TemplateRef) {
					return false
				}
			}
		}
		state[t] = 2
		return true
	}
	for _, t := range templates {
		if !visit(t, 0) {
			return false
		}
	}
	return true
}
