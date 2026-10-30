package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestPendingCreateCleanupSurvivesRecoveryAndPreemption(t *testing.T) {
	for _, mode := range []string{"restart", "cancel", "supersede", "already-completed"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now().UTC()
			s := testStore(t, &now)
			ctx := context.Background()
			repo := createRepo(t, s)
			spec := testRunSpec(repo.ID, "feature/pending", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			if _, err := s.EnqueueRun(ctx, spec); err != nil {
				t.Fatal(err)
			}
			run, err := s.ClaimNextRun(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.SetRunJobName(ctx, run.ID, "wf"); err != nil {
				t.Fatal(err)
			}
			if mode == "already-completed" {
				canceled, err := s.CancelRun(ctx, model.RunCancelRequest{RunID: run.ID, Actor: "operator"})
				if err != nil {
					t.Fatal(err)
				}
				old := canceled.Cancellations[0]
				if err := s.CompleteRunCancellation(ctx, old.RunID, old.JobName); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.MarkRunCreatePending(ctx, run.ID, "foreign-name"); !errors.Is(err, ErrInvalidState) {
				t.Fatal("wrong job intent accepted", err)
			}
			if err := s.MarkRunCreatePending(ctx, run.ID, "wf"); err != nil {
				t.Fatal(err)
			}
			old, _ := s.PendingRunCancellations(ctx)
			if len(old) != 1 {
				t.Fatal(old)
			}
			now = now.Add(time.Second)
			switch mode {
			case "cancel":
				if _, err := s.CancelRun(ctx, model.RunCancelRequest{RunID: run.ID, Actor: "operator"}); err != nil {
					t.Fatal(err)
				}
			case "supersede":
				spec.SHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				if _, err := s.EnqueueRun(ctx, spec); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.RecoverOwnerState(ctx); err != nil {
				t.Fatal(err)
			}
			current, _ := s.Run(ctx, run.ID)
			pending, _ := s.PendingRunCancellations(ctx)
			if len(pending) != 1 || pending[0].Reason != "owner_restart" || pending[0].JobName != "wf" {
				t.Fatalf("restart/preemption lost cleanup: %+v", pending)
			}
			if mode == "restart" && (current.Status != model.RunRunning || current.Phase != "infrastructure-pending") {
				t.Fatalf("restart freed pending run: %+v", current)
			}
			if err := s.MarkRunCreatePending(ctx, run.ID, "wf"); err != nil {
				t.Fatal(err)
			}
			if err := s.CompleteObservedRunCancellation(ctx, old[0]); !errors.Is(err, ErrInvalidState) {
				t.Fatalf("stale NotFound retired re-armed cancellation: %v", err)
			}
			pending, _ = s.PendingRunCancellations(ctx)
			if len(pending) != 1 {
				t.Fatal("stale completion lost obligation")
			}
			for range 2 {
				if err := s.CompleteObservedRunCancellation(ctx, pending[0]); err != nil {
					t.Fatal("current completion not idempotent", err)
				}
			}
		})
	}
}
