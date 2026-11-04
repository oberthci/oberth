package store

import (
	"fmt"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestActiveSafetySelectionHasIndependentOverflowFence(t *testing.T) {
	s, run, _ := resourceStoreFixture(t)
	if _, err := s.PutStepResult(t.Context(), model.StepResult{RunID: run.ID, Stage: "build", Step: "unexecuted", Status: model.StepSkipped, DeclaredSize: "M"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}
	for n := range 101 {
		// Separate refs keep ordinary enqueue supersession from replacing the
		// active population whose query bound is under test.
		if _, err := s.EnqueueRun(t.Context(), testRunSpec(run.RepoID, fmt.Sprintf("active-bound-%d", n), fmt.Sprintf("%040x", n+1))); err != nil {
			t.Fatal(err)
		}
		h, err := s.RunResources(t.Context(), run.RepoID, RunResourceFilter{Ref: "any", Runs: 1})
		if err != nil || h.SafetyIncomplete != (n == 100) || len(h.SafetyRuns) != 1+min(n+1, 100) {
			t.Fatalf("active bound n=%d: %+v %v", n, h, err)
		}
	}
}
