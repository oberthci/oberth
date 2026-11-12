package store

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestOwnerRestartPreservesWaitingQueue(t *testing.T) {
	for _, named := range []bool{false, true} {
		name := "before-checkout"
		if named {
			name = "waiting-for-admission"
		}
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
			path := filepath.Join(t.TempDir(), "queue.db")
			s := openTestStore(t, path, &now)
			repo := createRepo(t, s)
			ctx := t.Context()
			var originals []model.Run
			var promotion model.Promotion
			for _, kind := range []string{"branch", "tag", "release", "promotion"} {
				spec := testRunSpec(repo.ID, "feature/waiting", strings.Repeat("a", 40))
				if kind == "tag" || kind == "release" {
					spec.RefKind, spec.Ref, spec.Trigger = model.RefTag, "v1-"+kind, kind
					spec.Release = kind == "release"
				}
				var enqueued model.EnqueueRunResult
				var err error
				if kind == "promotion" {
					spec.Ref, spec.Trigger, spec.BaseSHA = "promotion/main/aaaaaaaaaaaa", kind, strings.Repeat("b", 40)
					enqueued, promotion, err = s.EnqueuePromotionRun(ctx, spec, model.PromotionSpec{
						RepoID: repo.ID, SourceBranch: "feature/promote", SourceSHA: spec.SHA,
						TargetRef: "main", PreviousSHA: spec.BaseSHA, ResultSHA: spec.SHA, Actor: spec.Actor,
					})
				} else {
					enqueued, err = s.EnqueueRun(ctx, spec)
				}
				requireRestartNoError(t, err)
				claimed, err := s.ClaimNextRun(ctx)
				requireRestartNoError(t, err)
				if claimed.ID != enqueued.ID || claimed.StartedAt != nil {
					t.Fatalf("unexpected claim: %+v", claimed)
				}
				if named {
					_, err = s.SetRunWorkflowName(ctx, claimed.ID, "workflow-"+kind)
					requireRestartNoError(t, err)
					requireRestartNoError(t, s.RecordAdmissionRequested(ctx, claimed.ID))
				}
				originals = append(originals, claimed)
			}
			// An unclaimed run must retain its position too.
			later, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/later", strings.Repeat("c", 40)))
			requireRestartNoError(t, err)
			requireRestartNoError(t, s.Close())
			now = now.Add(time.Minute)
			s = openTestStore(t, path, &now)
			// Repeated owner recovery must neither duplicate nor interrupt work.
			requireRestartNoError(t, s.RecoverOwnerState(ctx))
			for _, original := range append(originals, later.Run) {
				current, err := s.Run(ctx, original.ID)
				requireRestartNoError(t, err)
				if current.Status != model.RunQueued || current.ID != original.ID ||
					current.QueueSequence != original.QueueSequence || current.AdmissionSequence != original.AdmissionSequence ||
					!current.QueuedAt.Equal(original.QueuedAt) ||

					current.SupersededBy != "" || current.StartedAt != nil || current.FinishedAt != nil || current.Error != "" {
					t.Errorf("restart changed waiting identity/order/priority: before=%+v after=%+v", original, current)
				}
			}
			currentPromotion, err := s.Promotion(ctx, promotion.ID)
			requireRestartNoError(t, err)
			if currentPromotion != promotion {
				t.Errorf("restart changed pending promotion: before=%+v after=%+v", promotion, currentPromotion)
			}
			counts, err := s.CountRuns(ctx)
			requireRestartNoError(t, err)
			if counts.Total != 5 {
				t.Errorf("restart created runs: %+v", counts)
			}
			pending, err := s.PendingRunCancellations(ctx)
			requireRestartNoError(t, err)
			if len(pending) != 0 {
				t.Errorf("unsubmitted waiting work needs no Workflow deletion: %+v", pending)
			}
			// Highest-priority work keeps admission order: the oldest tag,
			// the next release, then the promotion. The bumped branch still
			// leads ordinary queued work.
			for _, index := range []int{0, 1, 2, 3, 4} {
				want := append(originals, later.Run)[index]
				claimed, err := s.ClaimNextRun(ctx)
				if err != nil || claimed.ID != want.ID {
					t.Fatalf("queue order: got %s (%v), want %s", claimed.ID, err, want.ID)
				}
				_, err = s.MarkRunAdmitted(ctx, claimed.ID)
				requireRestartNoError(t, err)
			}
		})
	}
}

func TestOwnerRestartKeepsAdmittedRecovery(t *testing.T) {
	for _, kind := range []string{"branch", "tag", "release", "promotion"} {
		for _, named := range []bool{false, true} {
			name := kind + "/before-submission"
			if named {
				name = kind + "/with-workflow"
			}
			t.Run(name, func(t *testing.T) {
				now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
				s := testStore(t, &now)
				repo := createRepo(t, s)
				ctx := t.Context()
				spec := testRunSpec(repo.ID, "feature/running", strings.Repeat("a", 40))
				var promotion model.Promotion
				var err error
				if kind == "promotion" {
					spec.Trigger, spec.Ref, spec.BaseSHA = kind, "promotion/main/aaaaaaaaaaaa", strings.Repeat("b", 40)
					_, promotion, err = s.EnqueuePromotionRun(ctx, spec, model.PromotionSpec{RepoID: repo.ID,
						SourceBranch: "feature/promote", SourceSHA: spec.SHA, TargetRef: "main",
						PreviousSHA: spec.BaseSHA, ResultSHA: spec.SHA, Actor: spec.Actor})
				} else {
					if kind != "branch" {
						spec.RefKind, spec.Ref, spec.Trigger, spec.Release = model.RefTag, "v1", kind, kind == "release"
					}
					_, err = s.EnqueueRun(ctx, spec)
				}
				requireRestartNoError(t, err)
				claimed, err := s.ClaimNextRun(ctx)
				requireRestartNoError(t, err)
				_, err = s.MarkRunAdmitted(ctx, claimed.ID)
				requireRestartNoError(t, err)
				if named {
					_, err = s.SetRunWorkflowName(ctx, claimed.ID, "workflow-running")
					requireRestartNoError(t, err)
					_, err = s.MarkRunSubmitted(ctx, claimed.ID)
					requireRestartNoError(t, err)
				}
				before, err := s.Run(ctx, claimed.ID)
				requireRestartNoError(t, err)
				now = now.Add(time.Minute)
				requireRestartNoError(t, s.RecoverOwnerState(ctx))
				after, err := s.Run(ctx, claimed.ID)
				requireRestartNoError(t, err)
				if named {
					if !reflect.DeepEqual(before, after) {
						t.Fatalf("submitted run changed before Workflow reconciliation: before=%+v after=%+v", before, after)
					}
				} else if kind == "promotion" {
					if after.Status != model.RunInterrupted || after.FinishedAt == nil {
						t.Fatalf("admitted promotion lost its interruption behavior: %+v", after)
					}
					current, err := s.Promotion(ctx, promotion.ID)
					requireRestartNoError(t, err)
					if current.Status != model.PromotionInterrupted {
						t.Fatalf("admitted promotion not interrupted: %+v", current)
					}
				} else if after.Status != model.RunQueued || after.Reason != RestartRequeueReason || after.QueueSequence != before.QueueSequence || after.AdmissionSequence != before.AdmissionSequence {
					t.Fatalf("admitted run lost its existing reset behavior: %+v", after)
				}
			})
		}
	}
}
