package app

import (
	"context"
	"io"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type adoptingArgoControl struct {
	configuredController
	request  argojob.AdoptionRequest
	binding  argojob.Adoption
	found    bool
	resumed  bool
	events   []runprogress.Event
	retained int64
}

func (controller *adoptingArgoControl) Adopt(_ context.Context, request argojob.AdoptionRequest) (argojob.Adoption, bool, error) {
	controller.request = request
	return controller.binding, controller.found, nil
}
func (controller *adoptingArgoControl) WaitResumed(_ context.Context, name, runID string, _ io.Writer, events []runprogress.Event, retained int64, binding argojob.Adoption) (argojob.Completion, error) {
	controller.resumed = true
	controller.events, controller.retained = events, retained
	if name != controller.request.Name || runID != controller.request.RunID || binding != controller.binding {
		panic("adopted intent changed")
	}
	return argojob.Completion{Name: name, Succeeded: true}, nil
}

func TestArgoJobsAdoptRestoresIntentAndResumesWait(t *testing.T) {
	controller := &adoptingArgoControl{found: true, binding: argojob.Adoption{Size: periapsis.L, UID: "workflow-uid", Identity: "spec-identity"}}
	jobs := &ArgoJobs{controller: controller, intents: map[string]argoIntent{}}
	run := model.Run{ID: "run", JobName: "workflow", RefKind: model.RefBranch, Ref: "feature/adopt", SHA: "pushed-sha", TestedSHA: "tested-sha", ConcurrencyGroup: "build", Trigger: "branch"}
	for range 2 {
		size, found, err := jobs.Adopt(context.Background(), run)
		if err != nil || !found || size != periapsis.L {
			t.Fatalf("adopt = %s %v %v", size, found, err)
		}
	}
	if controller.request.SHA != "tested-sha" || controller.request.Group != "build" || controller.request.RunTrigger != argoworkflow.RunTriggerBranch || controller.request.Trigger != periapsis.TriggerCI {
		t.Fatalf("adopt request = %#v", controller.request)
	}
	if _, _, err := jobs.Adopt(context.Background(), model.Run{ID: "foreign", JobName: run.JobName, Trigger: "branch"}); err == nil {
		t.Fatal("foreign intent replaced adopted run")
	}
	controller.request = argojob.AdoptionRequest{Name: run.JobName, RunID: run.ID}
	result, err := jobs.WaitResumed(context.Background(), run.JobName, io.Discard, []runprogress.Event{{Burn: "test", Step: "unit"}}, 123)
	if err != nil || result.Status != model.RunPassed || !controller.resumed || len(controller.events) != 1 || controller.retained != 123 {
		t.Fatalf("resumed Wait = %#v %v", result, err)
	}
	if len(jobs.intents) != 0 {
		t.Fatal("completed adopted intent retained")
	}
}

func TestArgoJobsAdoptAbsentWorkflowDoesNotCreateIntent(t *testing.T) {
	jobs := &ArgoJobs{controller: &adoptingArgoControl{}, intents: map[string]argoIntent{}}
	if _, found, err := jobs.Adopt(context.Background(), model.Run{ID: "run", JobName: "missing", Trigger: "branch"}); err != nil || found || len(jobs.intents) != 0 {
		t.Fatalf("missing adoption = %v %v %v", found, err, jobs.intents)
	}
}
