package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestOversizeTerminalObservationFinishesWithoutReplayEvidence(t *testing.T) {
	for _, kind := range []string{"body", "nodes"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Now()
			s := testStore(t, &now)
			repo := createRepo(t, s)
			run, err := s.EnqueueRun(t.Context(), model.RunSpec{RepoID: repo.ID, RefKind: model.RefTag, Ref: "v1", SHA: strings.Repeat("a", 40), Actor: "actor", Trigger: "tag", Release: true})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.ClaimNextRun(t.Context()); err != nil {
				t.Fatal(err)
			}
			_, err = s.SetRunWorkflowName(t.Context(), run.ID, "workflow")
			if err != nil {
				t.Fatal(err)
			}
			observation := &model.RunTerminalObservation{Version: 1, WorkflowName: "workflow", Nodes: 4096, NodeIDs: make([]string, 4096), Steps: []model.RunObservedStep{{Stage: strings.Repeat("s", 1<<20), Step: "leaf", Status: model.StepPassed}}}
			if kind == "nodes" {
				observation.Nodes = 8192
				observation.Steps = nil
			}
			finished, err := s.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunPassed, TerminalObservation: observation})
			if err != nil || finished.Status != model.RunPassed {
				t.Fatal("Argo verdict could not finish", finished.Status, err)
			}
			tx, err := s.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			evidence, err := replayAttemptTx(t.Context(), tx, finished)
			if err != nil || evidence.Observation != nil {
				t.Fatal("oversize observation retained replay authority", evidence.Observation, err)
			}
		})
	}
}
