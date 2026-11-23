// Package argoworkflowtest supplies one names-and-dependencies task graph for
// admission and display tests.
//
// Spec: docs/plans/c4-admission-rules-and-plan.md#purpose
package argoworkflowtest

import (
	_ "embed"
	"encoding/json"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

//go:embed testdata/cli-dag.json
var taskGraph []byte

// Plan prepares the task graph with inert leaf templates and no live reads.
func Plan(t *testing.T) admissioncontract.Plan {
	t.Helper()
	var tasks []wf.DAGTask
	if err := json.Unmarshal(taskGraph, &tasks); err != nil {
		t.Fatal(err)
	}
	for i := range tasks {
		tasks[i].Template = "leaf"
	}
	w := wf.Workflow{}
	w.APIVersion, w.Kind = "argoproj.io/v1alpha1", "Workflow"
	w.Spec.Entrypoint = "main"
	w.Spec.Templates = []wf.Template{{Name: "main", DAG: &wf.DAGTemplate{Tasks: tasks}}, {Name: "leaf", Suspend: &wf.SuspendTemplate{}}}
	raw, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := argoworkflow.Prepare(t.Context(), admissioncontract.PrepareRequest{
		Mode: admissioncontract.ModeInspect, Trigger: model.TriggerBranch,
		SelectedFile: ".oberth/build.yaml", Document: raw,
	}, argoworkflow.Policy{}, admissioncontract.PrepareHooks{OfflineValidation: true})
	if err != nil {
		t.Fatal(err)
	}
	return prepared.View().Plan
}
