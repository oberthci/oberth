package store

import (
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestTerminalRunStepEvidenceCannotBeRewritten(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "main", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	result := model.StepResult{RunID: run.ID, Stage: "test", Step: "unit", DeclaredSize: "S", Status: model.StepPassed}
	if _, err = s.PutStepResult(t.Context(), result); err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}
	result.Status = model.StepFailed
	result.ExitCode = 1
	if _, err = s.PutStepResult(t.Context(), result); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("terminal step changed: %v", err)
	}
	results, err := s.StepResults(t.Context(), run.ID)
	if err != nil || len(results) != 1 || results[0].Status != model.StepPassed {
		t.Fatalf("terminal evidence changed: %+v %v", results, err)
	}
}
