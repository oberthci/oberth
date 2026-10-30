package service

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type lostDatabaseAck struct {
	*store.Store
	operation string
	writes    atomic.Int64
}

func (s *lostDatabaseAck) lost(operation string, err error) error {
	if operation == s.operation {
		count := s.writes.Add(1)
		if err == nil && count == 1 {
			return context.DeadlineExceeded
		}
	}
	return err
}
func (s *lostDatabaseAck) ClaimNextRun(ctx context.Context) (model.Run, error) {
	v, e := s.Store.ClaimNextRun(ctx)
	return v, s.lost("ClaimNextRun", e)
}
func (s *lostDatabaseAck) RequeueStrandedRun(ctx context.Context, id string) (model.Run, error) {
	v, e := s.Store.RequeueStrandedRun(ctx, id)
	return v, s.lost("RequeueStrandedRun", e)
}
func (s *lostDatabaseAck) BeginPublication(ctx context.Context, spec model.PublicationSpec) (model.Publication, error) {
	v, e := s.Store.BeginPublication(ctx, spec)
	return v, s.lost("BeginPublication", e)
}
func (s *lostDatabaseAck) FinalizePublication(ctx context.Context, id string, status model.PublicationStatus, failure string) (model.PublicationFinalization, error) {
	v, e := s.Store.FinalizePublication(ctx, id, status, failure)
	return v, s.lost("FinalizePublication", e)
}
func (s *lostDatabaseAck) MarkRunAdmitted(ctx context.Context, id string) (model.Run, error) {
	v, e := s.Store.MarkRunAdmitted(ctx, id)
	return v, s.lost("MarkRunAdmitted", e)
}
func (s *lostDatabaseAck) MarkRunSubmitted(ctx context.Context, id string) (model.Run, error) {
	v, e := s.Store.MarkRunSubmitted(ctx, id)
	return v, s.lost("MarkRunSubmitted", e)
}
func (s *lostDatabaseAck) MarkRunParked(ctx context.Context, id string) (model.Run, error) {
	v, e := s.Store.MarkRunParked(ctx, id)
	return v, s.lost("MarkRunParked", e)
}

func TestDatabaseDeadlineLostClaimAckKeepsExactRun(t *testing.T) {
	fixture := newRecoveryFixture(t)
	first, err := fixture.store.EnqueueRun(t.Context(), model.RunSpec{RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: "one", SHA: strings.Repeat("a", 40), Actor: "test", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.store.EnqueueRun(t.Context(), model.RunSpec{RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: "two", SHA: strings.Repeat("b", 40), Actor: "test", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	scheduler := fixture.scheduler(t, &recoveryJobs{})
	fault := &lostDatabaseAck{Store: fixture.store, operation: "ClaimNextRun"}
	scheduler.store = fault
	run, err := scheduler.database().ClaimNextRun(deadlineScope(t.Context(), scheduler))
	if err != nil || run.ID != first.ID || fault.writes.Load() != 1 {
		t.Fatalf("lost original claim: %+v %v writes=%d", run, err, fault.writes.Load())
	}
	remaining, err := fixture.store.Run(t.Context(), second.ID)
	if err != nil || remaining.Status != model.RunQueued {
		t.Fatal("claimed a second run on retry", remaining, err)
	}
}

func TestDatabaseDeadlineLostTransitionAcksConverge(t *testing.T) {
	for _, operation := range []string{"RequeueStrandedRun", "BeginPublication", "FinalizePublication", "MarkRunAdmitted", "MarkRunSubmitted", "MarkRunParked"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			fixture := newRecoveryFixture(t)
			run := fixture.enqueueAndClaim(t, "feature", strings.Repeat("a", 40))
			scheduler := fixture.scheduler(t, &recoveryJobs{})
			fault := &lostDatabaseAck{Store: fixture.store, operation: operation}
			scheduler.store = fault
			database := scheduler.database()
			ctx := deadlineScope(t.Context(), scheduler)
			spec := model.PublicationSpec{RepoID: run.RepoID, RunID: run.ID, RefKind: run.RefKind, Ref: run.Ref, ResultSHA: run.SHA, Actor: run.Actor}
			switch operation {
			case "MarkRunAdmitted":
				current, err := database.MarkRunAdmitted(ctx, run.ID)
				if err != nil || current.StartedAt == nil {
					t.Fatal(current, err)
				}
			case "MarkRunSubmitted":
				fixture.setJobName(t, run.ID, "known-job")
				current, err := database.MarkRunSubmitted(ctx, run.ID)
				if err != nil || current.Phase != "running" {
					t.Fatal(current, err)
				}
			case "MarkRunParked":
				if _, err := fixture.store.MarkRunAdmitted(t.Context(), run.ID); err != nil {
					t.Fatal(err)
				}
				current, err := database.MarkRunParked(ctx, run.ID)
				if err != nil || current.StartedAt != nil {
					t.Fatal(current, err)
				}
			case "RequeueStrandedRun":
				fixture.setJobName(t, run.ID, "known-job")
				successor, err := database.RequeueStrandedRun(ctx, run.ID)
				if err != nil || successor.ID == "" || successor.ID == run.ID {
					t.Fatal(successor, err)
				}
				old, err := fixture.store.Run(t.Context(), run.ID)
				if err != nil || old.SupersededBy != successor.ID {
					t.Fatal(old, err)
				}
			case "BeginPublication":
				publication, err := database.BeginPublication(ctx, spec)
				if err != nil || publication.ID == "" {
					t.Fatal(publication, err)
				}
				pending, err := fixture.store.PendingPublications(t.Context())
				if err != nil || len(pending) != 1 || pending[0].ID != publication.ID {
					t.Fatal(pending, err)
				}
			case "FinalizePublication":
				publication, err := fixture.store.BeginPublication(t.Context(), spec)
				if err != nil {
					t.Fatal(err)
				}
				final, err := database.FinalizePublication(ctx, publication.ID, model.PublicationDelivered, "")
				if err != nil || final.Run.Status != model.RunPassed || final.Publication.Status != model.PublicationDelivered {
					t.Fatal(final, err)
				}
			}
			if fault.writes.Load() != 1 || scheduler.DatabaseRetries() != 1 {
				t.Fatalf("write replayed: writes=%d retries=%d", fault.writes.Load(), scheduler.DatabaseRetries())
			}
		})
	}
}

func TestDatabaseDeadlinePermanentStoreFailureStillStopsScheduler(t *testing.T) {
	for _, operation := range []string{"PendingRunCancellations", "ClaimNextRun"} {
		t.Run(operation, func(t *testing.T) {
			fixture := newRecoveryFixture(t)
			scheduler, _ := faultScheduler(t, fixture, &recoveryJobs{}, operation, store.ErrInvalidState)
			ctx, cancel := context.WithTimeout(t.Context(), testStartTimeout(5*time.Second))
			defer cancel()
			err := scheduler.Run(ctx)
			if err == nil || scheduler.DatabaseRetries() != 0 {
				t.Fatal("invariant did not reach fatal owner result", err, scheduler.DatabaseRetries())
			}
		})
	}
}
