package store

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestEvictionRetryRechecksDurableFailedAttempts(t *testing.T) {
	for _, kind := range []string{"eviction", "wrong workflow", "earlier failure", "other failed step", "missing proof", "unclassified failure"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Now().UTC()
			s := testStore(t, &now)
			repo := createRepo(t, s)
			source := autoRetrySource(t, s, repo.ID, "feature/eviction")
			if _, err := s.SetRunWorkflowName(t.Context(), source.ID, "workflow"); err != nil {
				t.Fatal(err)
			}
			observed := model.RunObservedStep{Stage: "ci", Step: "install-helm", Status: model.StepFailed, Started: true, Evicted: true, StartedAt: &now, FinishedAt: &now}
			observation := &model.RunTerminalObservation{Version: 1, WorkflowName: "workflow", Nodes: 2, Steps: []model.RunObservedStep{observed}}
			step := model.StepResult{RunID: source.ID, Stage: observed.Stage, Step: observed.Step, Status: model.StepFailed, DeclaredSize: "M", StartedAt: &now, FinishedAt: &now}
			switch kind {
			case "wrong workflow":
				observation.WorkflowName = "foreign"
			case "earlier failure":
				earlier := now.Add(-time.Minute)
				step.StartedAt = &earlier
			case "other failed step":
				step.Step = "unit"
			case "missing proof":
				observation = nil
			case "unclassified failure":
				observation.Steps[0].Evicted = false
			}
			if _, err := s.PutStepResult(t.Context(), step); err != nil {
				t.Fatal(err)
			}
			result := autoRetryResult()
			result.Error = "Evicted"
			result.TerminalObservation = observation
			failed, err := s.FinishRun(t.Context(), source.ID, result)
			if err != nil {
				t.Fatal(err)
			}
			if (failed.AutoRetriedBy != "") != (kind == "eviction") {
				t.Fatalf("retry decision: %+v", failed)
			}
		})
	}
}
