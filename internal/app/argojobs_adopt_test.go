package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type adoptingArgoControl struct {
	configuredController
	request  argojob.AdoptionRequest
	binding  argojob.Adoption
	found    bool
	adoptErr error
	waitErr  error
	resumed  bool
	events   []runprogress.Event
	retained int64
}

func (controller *adoptingArgoControl) Adopt(_ context.Context, request argojob.AdoptionRequest) (argojob.Adoption, bool, error) {
	controller.request = request
	return controller.binding, controller.found, controller.adoptErr
}
func (controller *adoptingArgoControl) WaitResumed(_ context.Context, name, runID string, _ io.Writer, events []runprogress.Event, retained int64, binding argojob.Adoption) (argojob.Completion, error) {
	controller.resumed = true
	controller.events, controller.retained = events, retained
	if name != controller.request.Name || runID != controller.request.RunID || binding != controller.binding {
		panic("adopted intent changed")
	}
	return argojob.Completion{Name: name, Succeeded: true}, controller.waitErr
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

func TestArgoJobsAdoptedTerminalObservationErrorStillCollectsArtifacts(t *testing.T) {
	budget := errors.New("step test/unit breached the aggregate run log budget")
	controller := &adoptingArgoControl{found: true, waitErr: budget, binding: argojob.Adoption{Size: periapsis.L, UID: "workflow-uid", Identity: "spec-identity"}}
	collector, artifacts := &fakeCollector{body: []byte("archive")}, &fakeArtifactStore{}
	jobs := &ArgoJobs{controller: controller, intents: map[string]argoIntent{}, collector: collector, artifacts: artifacts, artifactLimit: 1 << 20}
	run := model.Run{ID: "run", JobName: "workflow", RefKind: model.RefBranch, Ref: "feature/adopt", SHA: "sha", Trigger: "branch"}
	if _, _, err := jobs.Adopt(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	result, err := jobs.WaitResumed(context.Background(), run.JobName, io.Discard, nil, 0)
	if !errors.Is(err, budget) || result.Status != model.RunPassed {
		t.Fatalf("terminal observation error = %#v %v", result, err)
	}
	if collector.calls != 1 || string(artifacts.stored) != "archive" {
		t.Fatalf("terminal Workflow artifacts skipped: collector=%d stored=%q", collector.calls, artifacts.stored)
	}
}

func TestArgoJobsAdoptMarksRefusalButNotReadFailure(t *testing.T) {
	run := model.Run{ID: "run", JobName: "workflow", RefKind: model.RefBranch, Ref: "feature/adopt", SHA: "sha", Trigger: "branch"}
	for _, test := range []struct {
		err     error
		refused bool
	}{
		{fmt.Errorf("%w: argojob: Workflow workflow adoption binding mismatch", argojob.ErrAdoptionRefused), true},
		{errors.New("argojob: read Workflow for adoption: connection refused"), false},
	} {
		jobs := &ArgoJobs{controller: &adoptingArgoControl{adoptErr: test.err}, intents: map[string]argoIntent{}}
		_, found, err := jobs.Adopt(context.Background(), run)
		if err == nil || found || errors.Is(err, service.ErrAdoptionRefused) != test.refused || len(jobs.intents) != 0 {
			t.Fatalf("adopt(%v) = %v %v intents=%v", test.err, found, err, jobs.intents)
		}
	}
}
