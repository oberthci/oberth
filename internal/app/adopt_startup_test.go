package app

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
)

func TestArgoStartupCompletedAdoptionUsesResumedWait(t *testing.T) {
	controller := &adoptingArgoControl{found: true, binding: argojob.Adoption{UID: "workflow-uid", Identity: "spec-identity"}}
	collector, artifacts := &fakeCollector{body: []byte("archive")}, &fakeArtifactStore{}
	jobs := &ArgoJobs{controller: controller, intents: map[string]argoIntent{}, collector: collector, artifacts: artifacts, artifactLimit: 1 << 20}
	run := model.Run{ID: "run", WorkflowName: "workflow", RefKind: model.RefBranch, Ref: "feature/adopt", SHA: "sha", Trigger: "branch"}
	for range 2 {
		if found, err := jobs.Adopt(t.Context(), run); err != nil || !found {
			t.Fatal(found, err)
		}
	}
	if collector.calls != 0 || controller.resumed {
		t.Fatal("adoption performed terminal effects")
	}
	result, err := jobs.WaitResumed(t.Context(), run.WorkflowName, io.Discard)
	if err != nil || result.Status != model.RunPassed || len(jobs.intents) != 0 || collector.calls != 1 || string(artifacts.stored) != "archive" || !controller.resumed {
		t.Fatal(result, err, jobs.intents, collector.calls)
	}
}

func TestArgoStartupWatchUnavailableKeepsRetryClass(t *testing.T) {
	controller := &adoptingArgoControl{adoptErr: fmt.Errorf("watch: %w", argojob.ErrAdoptionUnavailable)}
	jobs := &ArgoJobs{controller: controller, intents: map[string]argoIntent{}}
	found, err := jobs.Adopt(t.Context(), model.Run{ID: "run", WorkflowName: "workflow", RefKind: model.RefBranch, Ref: "feature/adopt", SHA: "sha", Trigger: "branch"})
	if found || !errors.Is(err, service.ErrUnavailable) || !errors.Is(err, argojob.ErrAdoptionUnavailable) || errors.Is(err, service.ErrAdoptionRefused) {
		t.Fatal(found, err)
	}
}
