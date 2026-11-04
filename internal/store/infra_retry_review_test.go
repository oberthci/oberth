package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestInfrastructureRetryExecutionDeadlineRechecksDurableStartEvidence(t *testing.T) {
	for _, evidence := range []string{"empty", "passed", "started", "logs", "observed-started", "observed-running", "observed-passed", "observed-queued"} {
		for _, deadline := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/deadline=%v", evidence, deadline), func(t *testing.T) {
				now := time.Now()
				s := testStore(t, &now)
				repo := createRepo(t, s)
				source := autoRetrySource(t, s, repo.ID, "feature/deadline")
				result := autoRetryResult()
				result.Phase = "job"
				if deadline {
					result.Error = model.ExecutionDeadlineMessage
				}
				switch evidence {
				case "passed", "started", "logs":
					step := model.StepResult{RunID: source.ID, Stage: "ci", Step: "unit", DeclaredSize: "M", Status: model.StepPassed}
					if evidence == "started" {
						step.Status = model.StepSkipped
						step.StartedAt = &now
					}
					if evidence == "logs" {
						step.Status, step.LogEnd = model.StepSkipped, 42
					}
					if _, err := s.PutStepResult(t.Context(), step); err != nil {
						t.Fatal(err)
					}
				case "observed-started", "observed-running", "observed-passed", "observed-queued":
					step := model.RunObservedStep{Stage: "ci", Step: "unit", Status: model.StepQueued}
					switch evidence {
					case "observed-started":
						step.Started = true
					case "observed-running":
						step.Status = model.StepRunning
					case "observed-passed":
						step.Status = model.StepPassed
					}
					result.TerminalObservation = &model.RunTerminalObservation{Version: 1, Steps: []model.RunObservedStep{step}}
				}
				failed, err := s.FinishRun(t.Context(), source.ID, result)
				want := !deadline || evidence == "empty" || evidence == "observed-queued"
				if err != nil || (failed.AutoRetriedBy != "") != want {
					t.Fatalf("deadline retry=%+v want=%v err=%v", failed, want, err)
				}
			})
		}
	}
}
