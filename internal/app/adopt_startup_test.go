package app

import (
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
)

func TestArgoStartupResultConsumesOnlyLiveTerminalAdoption(t *testing.T) {
	controller := &adoptingArgoControl{found: true, binding: argojob.Adoption{UID: "workflow-uid", Identity: "spec-identity"}}
	collector, artifacts := &fakeCollector{body: []byte("archive")}, &fakeArtifactStore{}
	jobs := &ArgoJobs{controller: controller, intents: map[string]argoIntent{}, collector: collector, artifacts: artifacts, artifactLimit: 1 << 20}
	run := model.Run{ID: "run", WorkflowName: "workflow", RefKind: model.RefBranch, Ref: "feature/adopt", SHA: "sha", Trigger: "branch"}
	if found, err := jobs.Adopt(t.Context(), run); err != nil || !found {
		t.Fatal(found, err)
	}
	if _, err := jobs.AdoptedResult(t.Context(), run); !errors.Is(err, service.ErrJobNotTerminal) || collector.calls != 0 {
		t.Fatal("running adoption triggered terminal work", err)
	}
	controller.binding.Terminal = &argojob.Completion{Name: run.WorkflowName, Succeeded: true, Terminal: true}
	if found, err := jobs.Adopt(t.Context(), run); err != nil || !found {
		t.Fatal("settlement changed immutable adoption identity", found, err)
	}
	result, err := jobs.AdoptedResult(t.Context(), run)
	if err != nil || result.Status != model.RunPassed || len(jobs.intents) != 0 || collector.calls != 1 || string(artifacts.stored) != "archive" || controller.resumed {
		t.Fatal("startup terminal handoff", result, err, jobs.intents, collector.calls)
	}
}
