package argoworkflow

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func TestDefaultGraphsRefused(t *testing.T) {
	for _, kind := range []string{"dag", "steps"} {
		t.Run(kind, func(t *testing.T) {
			wf := decodeAdmissible(t, nil)
			defaults := &wfv1.Template{}
			if kind == "dag" {
				defaults.DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "extra", Template: "main"}}}
			} else {
				defaults.Steps = []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "extra", Template: "main"}}}}
			}
			wf.Spec.TemplateDefaults = defaults
			if err := Admit(wf, Policy{}); err == nil || !strings.Contains(err.Error(), "spec.templateDefaults."+kind+" is not admitted") {
				t.Fatal("default graph admitted", err)
			}
		})
	}
}

func TestDefaultPodFieldsRemainAdmitted(t *testing.T) {
	for _, kind := range []string{"container", "script", "metadata"} {
		t.Run(kind, func(t *testing.T) {
			wf := decodeAdmissible(t, nil)
			defaults := &wfv1.Template{}
			switch kind {
			case "container":
				defaults.Container = wf.Spec.Templates[0].Container.DeepCopy()
			case "script":
				defaults.Script = &wfv1.ScriptTemplate{Container: *wf.Spec.Templates[0].Container.DeepCopy(), Source: "true"}
			case "metadata":
				defaults.Metadata.Labels = map[string]string{"example.com/team": "ci"}
			}
			wf.Spec.TemplateDefaults = defaults
			if err := Admit(wf, Policy{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
