package store

import (
	"errors"
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
					!current.QueuedAt.Equal(original.QueuedAt) || current.StartedAt != nil || current.FinishedAt != nil || current.Error != "" {
					t.Errorf("restart changed waiting identity/order/priority: before=%+v after=%+v", original, current)
				}
			}
			currentPromotion, err := s.Promotion(ctx, promotion.ID)
			requireRestartNoError(t, err)
			wantPromotion := model.PromotionFailed
			if named {
				wantPromotion = model.PromotionPending
			}
			if currentPromotion.Status != wantPromotion || currentPromotion.RunID != promotion.RunID {
				t.Fatal(currentPromotion)
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
			claimed, err := s.ClaimNextRun(ctx)
			if err != nil || claimed.ID != later.ID {
				t.Fatal("claimed run was resubmitted", claimed, err)
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
				_, err = s.MarkRunSubmissionStarted(ctx, claimed.ID)
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
					if after.Status != model.RunFailed || after.FinishedAt == nil {
						t.Fatalf("admitted promotion lost its interruption behavior: %+v", after)
					}
					current, err := s.Promotion(ctx, promotion.ID)
					requireRestartNoError(t, err)
					if current.Status != model.PromotionFailed {
						t.Fatalf("admitted promotion not interrupted: %+v", current)
					}
				} else if after.Status != model.RunFailed || after.Error != model.ServerRestartReason || after.QueueSequence != before.QueueSequence || after.AdmissionSequence != before.AdmissionSequence {
					t.Fatalf("admitted run lost its existing reset behavior: %+v", after)
				}
			})
		}
	}
}

func TestServerRestartRunSupportsExplicitRetryAndRerun(t *testing.T) {
	for _, kind := range []model.RefKind{model.RefBranch, model.RefTag} {
		for _, operation := range []string{"retry", "rerun"} {
			t.Run(string(kind)+"/"+operation, func(t *testing.T) {
				now := time.Now()
				s := testStore(t, &now)
				repo := createRepo(t, s)
				spec := testRunSpec(repo.ID, "feature/restart", strings.Repeat("a", 40))
				spec.RefKind = kind
				if kind == model.RefTag {
					spec.Ref = "v1-restart"
					spec.Trigger = "tag"
				}
				source, err := s.EnqueueRun(t.Context(), spec)
				requireRestartNoError(t, err)
				_, err = s.ClaimNextRun(t.Context())
				requireRestartNoError(t, err)
				requireRestartNoError(t, s.RecoverOwnerState(t.Context()))
				failed, err := s.Run(t.Context(), source.ID)
				requireRestartNoError(t, err)
				if failed.Status != model.RunFailed || failed.Error != model.ServerRestartReason {
					t.Fatal(failed)
				}
				if _, err := s.ClaimNextRun(t.Context()); !errors.Is(err, ErrNotFound) {
					t.Fatal("restart automatically queued work", err)
				}
				audit := &model.RunReplayAudit{Basis: "pre-intent"}
				authorize := func(run model.Run, evidence model.RunReplayEvidence) error {
					if run.ID != source.ID || run.Error != model.ServerRestartReason || !evidence.ChainComplete || !evidence.Newest || evidence.PublicationStarted {
						t.Fatal(run, evidence)
					}
					audit.RunsChecked = len(evidence.Chain)
					audit.StepsChecked = len(evidence.Steps)
					return nil
				}
				var next model.Run
				if operation == "retry" {
					if kind == model.RefTag {
						if _, err := s.RetryRun(t.Context(), model.RunRetryRequest{RunID: source.ID, Actor: "operator"}); !errors.Is(err, ErrInvalidState) {
							t.Fatal("tag retry bypassed authorization", err)
						}
					}
					result, err := s.RetryRun(t.Context(), model.RunRetryRequest{RunID: source.ID, Actor: "operator", ReplayAudit: audit, Authorize: authorize})
					requireRestartNoError(t, err)
					next = result.Run
				} else {
					if kind == model.RefTag {
						if _, err := s.RerunRun(t.Context(), model.RunRerunRequest{RunID: source.ID, Actor: "operator"}); !errors.Is(err, ErrInvalidState) {
							t.Fatal("tag rerun bypassed authorization", err)
						}
					}
					result, err := s.RerunRun(t.Context(), model.RunRerunRequest{RunID: source.ID, Actor: "operator", Reason: "retry after restart", ReplayAudit: audit, Authorize: func(run model.Run, evidence model.RunReplayEvidence) (string, error) {
						return "", authorize(run, evidence)
					}})
					requireRestartNoError(t, err)
					next = result.Enqueued.Run
				}
				if next.ID == source.ID || next.Status != model.RunQueued || next.RefKind != kind || next.Ref != source.Ref || next.SHA != source.SHA || next.TestedSHA != source.TestedSHA || next.Actor != "operator" || next.Error != "" {
					t.Fatal(next)
				}
				unchanged, err := s.Run(t.Context(), source.ID)
				requireRestartNoError(t, err)
				if unchanged.Status != model.RunFailed || unchanged.Error != model.ServerRestartReason {
					t.Fatal("explicit replay rewrote failed source", unchanged)
				}
				claimed, err := s.ClaimNextRun(t.Context())
				requireRestartNoError(t, err)
				if claimed.ID != next.ID {
					t.Fatal(claimed)
				}
				count, err := s.CountRuns(t.Context())
				requireRestartNoError(t, err)
				if count.Total != 2 {
					t.Fatal(count)
				}
			})
		}
	}
}
