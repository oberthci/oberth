package store

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestFinishCancellationPreservesReasonWithoutNewError(t *testing.T) {
	const original = "cancelled by operator: stop requested"
	const stopped = "stopped by operator: Stopped with strategy 'Stop'"
	for _, running := range []bool{false, true} {
		for _, tc := range []struct {
			name, message, want string
			failed              bool
		}{
			{name: "empty", want: original},
			{name: "whitespace", message: "  ", want: original},
			{name: "native stop", message: stopped, want: stopped},
			{name: "ordinary failure", message: "exit code 23", want: original, failed: true},
		} {
			t.Run(map[bool]string{false: "queued", true: "running"}[running]+"/"+tc.name, func(t *testing.T) {
				now := time.Now()
				s := testStore(t, &now)
				repo := createRepo(t, s)
				run := cancelTestRun(t, s, repo, "feature/cancel-finish", "a")
				if running {
					if _, err := s.ClaimNextRun(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := s.db.Exec(`UPDATE runs SET reason=? WHERE id=?`, original, run.ID); err != nil {
					t.Fatal(err)
				}
				status, phase := model.RunInterrupted, model.PhaseCancelled
				if tc.failed {
					status, phase = model.RunFailed, "job"
				}
				got, err := s.FinishRun(t.Context(), run.ID, model.RunResult{Status: status, Phase: phase, Error: tc.message})
				if err != nil {
					t.Fatal(err)
				}
				var reason string
				if err = s.db.QueryRow(`SELECT reason FROM runs WHERE id=?`, run.ID).Scan(&reason); err != nil {
					t.Fatal(err)
				}
				if reason != tc.want || got.Reason != tc.want || got.Error != tc.message || got.Status != status || got.Phase != phase {
					t.Fatalf("finish lost cancellation context: raw=%q run=%+v", reason, got)
				}
			})
		}
	}
}
