package argoworkflow

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	core "k8s.io/api/core/v1"
	"testing"
)

func TestDeltaAdmissionNamespaceParity(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"env", "mount"} {
		t.Run(shape, func(t *testing.T) {
			w := simpleWorkflow()
			if shape == "env" {
				w.Spec.Templates[0].Container.Env = []core.EnvVar{{Name: "OBERTH_HOLD_CUSTOM", Value: "ordinary"}}
			} else {
				w.Spec.Templates[0].Volumes = []core.Volume{{Name: "scratch", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{}}}}
				w.Spec.Templates[0].Container.VolumeMounts = []core.VolumeMount{{Name: "scratch", MountPath: "/oberth-hold/work"}}
			}
			if _, err := Prepare(t.Context(), requestFor(t, w), Policy{}, identityHooks()); err != nil {
				t.Fatalf("ordinary main-admitted workflow changed admission: %v", err)
			}
		})
	}
}
func TestDeltaTerminationOutputIsEligible(t *testing.T) {
	t.Parallel()
	w := simpleWorkflow()
	leaf := w.Spec.Templates[0].DeepCopy()
	leaf.Name = "produce"
	leaf.Container.Command = []string{"sh", "-c", "printf original > /result; exit 7"}
	leaf.Container.TerminationMessagePath = "/result"
	leaf.Outputs.Parameters = []wf.Parameter{{Name: "result", ValueFrom: &wf.ValueFrom{Path: "/result"}}}
	w.Spec.Templates = []wf.Template{
		{Name: "main", DAG: &wf.DAGTemplate{Tasks: []wf.DAGTask{
			{Name: "produce", Template: "produce", ContinueOn: &wf.ContinueOn{Failed: true}},
			{Name: "verify", Template: "verify", Dependencies: []string{"produce"}, Arguments: wf.Arguments{Parameters: []wf.Parameter{{Name: "result", Value: wf.AnyStringPtr("{{tasks.produce.outputs.parameters.result}}")}}}},
		}}},
		*leaf,
		{Name: "verify", Inputs: wf.Inputs{Parameters: []wf.Parameter{{Name: "result"}}}, Container: &core.Container{Image: "example.test/image:1", Command: []string{"sh", "-c", `case "$1" in *"oberth-hold: command exited 7"*) exit 0;; *) exit 1;; esac`}, Args: []string{"sh", "{{inputs.parameters.result}}"}}},
	}
	p, err := Prepare(t.Context(), requestFor(t, w), Policy{}, identityHooks())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.View().Plan.Entries) != 2 || !p.View().Plan.Entries[0].HoldEligible {
		t.Fatal("counterexample is not admitted and eligible", p.View().Plan)
	}
	t.Log("admitted, hold-eligible output parameter aliases helper termination path")
}
