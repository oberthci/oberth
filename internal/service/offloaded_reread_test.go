package service

import (
	"errors"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"strings"
	"testing"
)

func TestSchedulerTerminalRereadKeepsOffloadedVerdictAndRows(t *testing.T) {
	for _, status := range []model.RunStatus{model.RunPassed, model.RunFailed} {
		t.Run(string(status), func(t *testing.T) {
			f := newRecoveryFixture(t)
			run := f.enqueueAndClaim(t, "main", strings.Repeat("a", 40))
			row := model.StepResult{RunID: run.ID, NodeID: "earlier-row", Step: "build", Status: model.StepPassed}
			if err := f.store.PutStepResults(t.Context(), run.ID, []model.StepResult{row}); err != nil {
				t.Fatal(err)
			}
			const reason = "argojob: step rows are unavailable because Argo node status is offloaded"
			jobs := &recoveryJobs{waitErr: errors.New("observation interrupted"), terminalResult: JobResult{Status: status, Phase: string(status), Error: reason}}
			scheduler := f.scheduler(t, jobs)
			if err := scheduler.executeAndCleanup(t.Context(), run); err != nil {
				t.Fatal(err)
			}
			final, err := f.store.Run(t.Context(), run.ID)
			if err != nil || final.Status != status || (status == model.RunFailed && final.Error != reason) || len(jobs.deleted) != 0 {
				t.Fatal("terminal native verdict lost", final, jobs.deleted, err)
			}
			if status == model.RunPassed {
				body, _, readErr := scheduler.logs.ReadRunLog(t.Context(), run.ID, runlog.Filter{})
				if readErr != nil || !strings.Contains(string(body), reason) {
					t.Fatal("publication discarded observation reason", string(body), readErr)
				}
			}
			rows, err := f.store.StepResults(t.Context(), run.ID)
			if err != nil || len(rows) != 1 || rows[0].NodeID != row.NodeID {
				t.Fatal("existing rows lost", rows, err)
			}
		})
	}
}
