package argoworkflow

import (
	"context"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

// The native harness consumes sealed preparation output for each executable shape.
func writeControllerSourceCopies(t *testing.T, directory string) {
	t.Helper()
	for _, kind := range []string{"container", "script", "container-set"} {
		w := sourceWorkflow()
		leaf := w.Spec.Templates[0].DeepCopy()
		leaf.Name = "work"
		leaf.Inputs.Artifacts = append(leaf.Inputs.Artifacts, pinInput(t, "library"))
		switch kind {
		case "script":
			leaf.Script = &wfv1.ScriptTemplate{Container: *leaf.Container, Source: "true"}
			leaf.Container = nil
		case "container-set":
			first, second := *leaf.Container, *leaf.Container
			first.Name, second.Name = "main", "check"
			leaf.ContainerSet = &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: first}, {Container: second}}}
			leaf.Container = nil
		}
		w.Spec.Templates = []wfv1.Template{{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "first", Template: "work"}, {Name: "second", Template: "work"}}}}, *leaf}
		request := requestFor(t, w)
		hooks := identityHooks()
		hooks.BindSource = func(_ context.Context, d sourcecontract.Descriptor) (sourcecontract.Binding, error) {
			return boundEvidence(request, d), nil
		}
		prepared, err := Prepare(t.Context(), request, Policy{}, hooks)
		if err != nil {
			t.Fatal(kind, err)
		}
		writeControllerJSON(t, directory, "source-copy-"+kind, prepared.View().Workflow)
	}
}
