package app

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

type schedulingControl struct {
	argoControl
	sha string
}

func (controller *schedulingControl) ObserveScheduling(_ context.Context, name, run, sha string) (*model.ExecutionObservation, error) {
	controller.sha = sha
	return &model.ExecutionObservation{State: "observed"}, nil
}
func TestArgoSchedulingUsesEffectiveTestedSHA(t *testing.T) {
	controller := &schedulingControl{}
	jobs := &ArgoJobs{controller: controller}
	for _, tested := range []string{"", "merged-tree"} {
		run := model.Run{SHA: "pushed-object", TestedSHA: tested, JobName: "wf", ID: "run"}
		if _, err := jobs.ObserveScheduling(t.Context(), run); err != nil {
			t.Fatal(err)
		}
		want := tested
		if want == "" {
			want = run.SHA
		}
		if controller.sha != want {
			t.Fatalf("observed SHA=%s want=%s", controller.sha, want)
		}
	}
}
