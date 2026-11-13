package argoworkflow

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func TestRunSourceReceiptRefusesExternalReferences(t *testing.T) {
	for _, name := range []string{"shared/pipeline@v1", "shared-template"} {
		for _, cluster := range []bool{false, true} {
			for _, site := range []string{"step", "dag", "hook", "inline", "workflow"} {
				t.Run(name+"/"+site+"/cluster="+map[bool]string{false: "false", true: "true"}[cluster], func(t *testing.T) {
					wf, err := Decode([]byte(admissibleDocument))
					if err != nil {
						t.Fatal(err)
					}
					ref := &wfv1.TemplateRef{Name: name, Template: "run", ClusterScope: cluster}
					main := &wf.Spec.Templates[0]
					switch site {
					case "step":
						main.Container = nil
						main.Steps = []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "call", TemplateRef: ref}}}}
					case "dag":
						main.Container = nil
						main.DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "call", TemplateRef: ref}}}
					case "hook":
						wf.Spec.Hooks = wfv1.LifecycleHooks{wfv1.ExitLifecycleEvent: {TemplateRef: ref}}
					case "inline":
						main.Container = nil
						main.Steps = []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "outer", Inline: &wfv1.Template{DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "call", TemplateRef: ref}}}}}}}}
					case "workflow":
						wf.Spec.WorkflowTemplateRef = &wfv1.WorkflowTemplateRef{Name: name, ClusterScope: cluster}
					}
					source, err := json.Marshal(wf)
					if err != nil {
						t.Fatal(err)
					}
					resolved, err := ResolveRunSource(source, RunSourceBinding{})
					if err != nil {
						t.Fatal(err)
					}
					_, err = resolved.Receipt(Policy{})
					var refusal *AdmissionError
					field := "templateRef"
					if site == "workflow" {
						field = "workflowTemplateRef"
					}
					if !errors.As(err, &refusal) || !strings.Contains(err.Error(), field) {
						t.Fatalf("external reference did not receive a document refusal: %v", err)
					}
				})
			}
		}
	}
}

func TestRunSourceReceiptPreservesAuthoredGraph(t *testing.T) {
	binding := RunSourceBinding{RunID: "run", RepositoryID: 1, SHA: strings.Repeat("a", 40), File: BuildFile, Trigger: RunTriggerBranch}
	resolved, err := ResolveRunSource([]byte(admissibleDocument), binding)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := resolved.Receipt(Policy{})
	if err != nil || !receipt.Summary().Valid() {
		t.Fatalf("ordinary authored graph did not receive a valid receipt: %v", err)
	}
	copy := receipt.Workflow()
	copy.Spec.Templates[0].Name = "changed"
	if receipt.Workflow().Spec.Templates[0].Name == "changed" || resolved.Workflow().Spec.Templates[0].Name == "changed" {
		t.Fatal("receipt exposed its admitted graph to caller mutation")
	}
}
