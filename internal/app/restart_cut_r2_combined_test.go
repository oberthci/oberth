package app

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// Shared partial-execution evidence must survive the immutable fallback after
// the intent is forgotten, both standalone and with RCA UX merged.
func TestRestartCutR2CombinedKeepsRunningEvidenceAndTerminalFallback(t *testing.T) {
	started := time.Now().Add(-time.Minute)
	failure := errors.New("executor identity delivery unavailable")
	c := &restartReadController{
		adoptingArgoControl: adoptingArgoControl{found: true, binding: argojob.Adoption{UID: "workflow-uid", Identity: "identity", Size: periapsis.M}},
		observed:            argojob.Completion{Name: "workflow", Phase: "Running", Reason: "Workflow running", ReplayAttempts: []argojob.StepResult{{Burn: "ci", Step: "test", Status: runprogress.StepRunning, StartedAt: &started}}},
		observationError:    failure,
		next:                argojob.Completion{Name: "workflow", Terminal: true, Interrupted: true, Infrastructure: true, Reason: "server restart interrupted ci/test: missing exit"},
	}
	collector, artifacts := &fakeCollector{body: []byte("archive")}, &fakeArtifactStore{}
	jobs := &ArgoJobs{controller: c, intents: map[string]argoIntent{}, collector: collector, artifacts: artifacts, artifactLimit: 1 << 20}
	run := model.Run{ID: "run", JobName: "workflow", RefKind: model.RefBranch, Ref: "feature", SHA: "sha", Trigger: "branch"}
	if _, found, err := jobs.Adopt(t.Context(), run); err != nil || !found {
		t.Fatal(found, err)
	}
	observed, err := jobs.WaitResumed(t.Context(), run.JobName, io.Discard, nil, 0)
	if !errors.Is(err, failure) || !observed.ObservedStepStart || observed.Reobserve == nil || collector.calls != 0 || len(jobs.intents) != 0 {
		t.Fatal("running evidence lost or live artifacts collected", observed, err, collector.calls)
	}
	final, err := observed.Reobserve(t.Context())
	if err != nil || final.Status != model.RunInterrupted || final.Phase != "interrupted" || final.FailedStep != "" {
		t.Fatal("partial running snapshot overrode cut", final, err)
	}
}
