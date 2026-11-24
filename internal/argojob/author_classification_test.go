package argojob_test

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
	"testing"
)

// This same test is run against main through a read-only Go overlay.
func TestAuthorFailuresMatchMainAfterHelpers(t *testing.T) {
	t.Parallel()
	for _, helper := range []string{"source", "secret"} {
		for _, podEvidence := range []bool{false, true} {
			w := nativeWorkflow()
			w.Status.Phase = wf.WorkflowFailed
			template := &w.Spec.Templates[0]
			if helper == "source" {
				template.Inputs.Artifacts = []wf.Artifact{{Name: "source", ArtifactLocation: wf.ArtifactLocation{Plugin: &wf.PluginArtifact{Name: "oberth"}}}}
			} else {
				template.InitContainers = []wf.UserContainer{{Container: core.Container{Name: "oberth-secret-install"}}}
				template.Container.Command = []string{"/run/oberth/bin/oberth", "secretstore", "exec", "--", "go", "test"}
			}
			n := nativeNode("one", ".build", wf.NodeFailed, 1)
			n.Outputs = &wf.Outputs{ExitCode: new("1")}
			w.Status.Nodes[n.ID] = n
			var pods []*core.Pod
			if podEvidence {
				p := nativePod(w, n, 1)
				initName := "init-artifact-oberth"
				if helper == "secret" {
					initName = "oberth-secret-install"
				}
				p.Status.InitContainerStatuses = []core.ContainerStatus{{Name: initName, State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 0}}}}
				p.Status.ContainerStatuses[0].State.Terminated.Message = "oberth:secret-injection-failed:v1"
				pods = []*core.Pod{p}
			}
			o := project(t, argojob.Projection{Workflow: w, Pods: pods, Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}})
			if o.Failure == nil || o.Failure.Class != stepmodel.FailureAuthor || o.Failure.Retry.Action != stepmodel.RetryNone || o.Steps[0].FailureClass != stepmodel.FailureAuthor {
				t.Fatal("author failure no longer matches main", helper, podEvidence, o)
			}
		}
	}
}
