package store

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestInfrastructureRetryPreservesOrdinaryAndAgedQueueOrder(t *testing.T) {
	for _, age := range []time.Duration{0, 2 * time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			now := time.Now().UTC()
			s := testStore(t, &now)
			repo := createRepo(t, s)
			source := autoRetrySource(t, s, repo.ID, "feature/first")
			now = now.Add(age + time.Second)
			other := autoRetrySource(t, s, repo.ID, "feature/other")
			failed, err := s.FinishRun(t.Context(), source.ID, autoRetryResult())
			if err != nil {
				t.Fatal(err)
			}
			retry, err := s.Run(t.Context(), failed.AutoRetriedBy)
			if err != nil || retry.AdmissionSequence != source.AdmissionSequence || !retry.QueuedAt.Equal(source.QueuedAt) || retry.QueueSequence <= other.QueueSequence {
				t.Fatalf("retry lost order or unique history identity: %+v %v", retry, err)
			}
			sourcePriority, _ := source.EffectivePriority(now)
			retryPriority, _ := retry.EffectivePriority(now)
			if sourcePriority != retryPriority {
				t.Fatalf("priority changed %s -> %s", sourcePriority, retryPriority)
			}
			claimed, err := s.ClaimNextRun(t.Context())
			if err != nil || claimed.ID != retry.ID {
				t.Fatalf("retry sent to back: %+v %v", claimed, err)
			}
		})
	}
}

func TestInfrastructureRetryRefusesTerminalObservationFailure(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	source := autoRetrySource(t, s, repo.ID, "feature/observed-failure")
	result := autoRetryResult()
	result.TerminalObservation = &model.RunTerminalObservation{Version: 1, WorkflowName: "wf", Nodes: 1, Steps: []model.RunObservedStep{{Stage: "ci", Step: "unit", Status: model.StepFailed, Started: true}}}
	failed, err := s.FinishRun(t.Context(), source.ID, result)
	if err != nil || failed.AutoRetriedBy != "" {
		t.Fatalf("observed failure replayed: %+v %v", failed, err)
	}
}

func TestInfrastructureRetryWaitsForPreflightCleanup(t *testing.T) {
	for _, cleaned := range []bool{false, true} {
		t.Run(fmt.Sprint(cleaned), func(t *testing.T) {
			now := time.Now()
			s := testStore(t, &now)
			intent := kvmIntent(t, s)
			if _, err := s.ReserveKVMPreflight(t.Context(), intent); err != nil {
				t.Fatal(err)
			}
			if cleaned {
				if err := s.CompleteKVMPreflightCleanup(t.Context(), intent.RunID, ""); err != nil {
					t.Fatal(err)
				}
			}
			failed, err := s.FinishRun(t.Context(), intent.RunID, autoRetryResult())
			if err != nil || (failed.AutoRetriedBy != "") != cleaned {
				t.Fatalf("cleanup eligibility %t: %+v %v", cleaned, failed, err)
			}
		})
	}
}

func TestInfrastructureRetryLegacyLineageSurvivesRestart(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	source := autoRetrySource(t, s, repo.ID, "feature/legacy")
	failed, err := s.FinishRun(t.Context(), source.ID, autoRetryResult())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE runs SET reason=? WHERE id=?`, "automatic infrastructure retry of "+source.ID, failed.AutoRetriedBy); err != nil {
		t.Fatal(err)
	}
	child, err := s.ClaimNextRun(t.Context())
	if err != nil || child.AutoRetryOf != source.ID {
		t.Fatal(child, err)
	}
	next, err := s.RequeueStrandedRun(t.Context(), child.ID)
	if err != nil || next.AutoRetryOf != source.ID {
		t.Fatalf("legacy lineage lost: %+v %v", next, err)
	}
	terminal, err := s.FinishRun(t.Context(), next.ID, autoRetryResult())
	if err != nil || terminal.AutoRetriedBy != "" {
		t.Fatalf("legacy retry budget reset: %+v %v", terminal, err)
	}
}

func TestInfrastructureRetryCannotOverrideCancellation(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	source := autoRetrySource(t, s, repo.ID, "feature/cancel")
	if _, err := s.CancelRun(t.Context(), model.RunCancelRequest{RunID: source.ID, Actor: "owner", Reason: "stop"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishRun(t.Context(), source.ID, autoRetryResult()); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("cancel overwritten: %v", err)
	}
	counts, err := s.CountRuns(t.Context())
	if err != nil || counts.Total != 1 {
		t.Fatal("cancel created retry", counts, err)
	}
}
