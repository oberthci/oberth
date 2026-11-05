package argoworkflow

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestAdmitRefusesNonleafDeadlines(t *testing.T) {
	for _, kind := range []string{"dag", "steps"} {
		for _, position := range []string{"named", "dag-inline", "steps-inline"} {
			for _, value := range []intstr.IntOrString{intstr.FromInt(7401), intstr.FromInt(0), intstr.FromString("{{workflow.parameters.deadline}}")} {
				t.Run(kind+"/"+position+"/"+value.String(), func(t *testing.T) {
					wf, err := Decode([]byte(admissibleDocument))
					if err != nil {
						t.Fatal(err)
					}
					leaf := wf.Spec.Templates[0]
					leaf.Name = "leaf"
					composite := wfv1.Template{Name: "main"}
					if kind == "dag" {
						composite.DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "run", Template: "leaf"}}}
					} else {
						composite.Steps = []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "run", Template: "leaf"}}}}
					}
					wf.Spec.Templates = []wfv1.Template{composite, leaf}
					target := &wf.Spec.Templates[0]
					location := "spec.templates[\"main\"]"
					if position != "named" {
						inline := composite.DeepCopy()
						inline.Name = ""
						parent := wfv1.Template{Name: "main"}
						if position == "dag-inline" {
							parent.DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "nested", Inline: inline}}}
							location += ".dag.tasks[\"nested\"].inline"
						} else {
							parent.Steps = []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "nested", Inline: inline}}}}
							location += ".steps[0][0].inline"
						}
						wf.Spec.Templates[0] = parent
						target = inline
					}
					if err := Admit(wf, Policy{}); err != nil {
						t.Fatalf("valid composite refused: %v", err)
					}
					target.ActiveDeadlineSeconds = &value
					err = Admit(wf, Policy{})
					want := location + ".activeDeadlineSeconds is only valid for leaf templates"
					if err == nil || !strings.Contains(err.Error(), want) {
						t.Fatalf("admission error = %v, want %q", err, want)
					}
				})
			}
		}
	}
}

func TestAdmitPreservesWorkflowAndLeafDeadlines(t *testing.T) {
	wf, err := Decode([]byte(admissibleDocument))
	if err != nil {
		t.Fatal(err)
	}
	deadline := intstr.FromInt(600)
	wf.Spec.Templates[0].ActiveDeadlineSeconds = &deadline
	if err := Admit(wf, Policy{}); err != nil {
		t.Fatal(err)
	}
	if wf.Spec.ActiveDeadlineSeconds == nil || *wf.Spec.ActiveDeadlineSeconds != 3600 || wf.Spec.Templates[0].ActiveDeadlineSeconds.IntVal != 600 {
		t.Fatal("admission changed a valid workflow or leaf deadline")
	}
}
