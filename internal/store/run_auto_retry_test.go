package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func autoRetryResult() model.RunResult {
	return model.RunResult{Status: model.RunFailed, Phase: "infrastructure", Error: "workflow unavailable", AutoRetryPreStep: true}
}
func autoRetrySource(t *testing.T, s *Store, repoID int64, ref string) model.Run {
	t.Helper()
	r, err := s.EnqueueRun(t.Context(), model.RunSpec{RepoID: repoID, RefKind: model.RefBranch, Ref: ref, SHA: strings.Repeat("a", 40), Actor: "pusher", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	return r.Run
}

func TestAutoRetryPreStepIsAtomicAuditedAndOnlyOnce(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	source := autoRetrySource(t, s, repo.ID, "feature/once")
	failed, err := s.FinishRun(t.Context(), source.ID, autoRetryResult())
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.Run(t.Context(), failed.AutoRetriedBy)
	if err != nil || failed.Status != model.RunFailed || failed.Error != "workflow unavailable" || next.AutoRetryOf != source.ID || next.Status != model.RunQueued || next.SHA != source.SHA || next.Actor != source.Actor || next.Credentialed {
		t.Fatalf("retry state: %+v %+v %v", failed, next, err)
	}
	action, err := s.LatestAuditActionByType(t.Context(), "run.retry.auto", "run")
	if err != nil || action.Actor != "oberth" || action.ResourceID != next.ID || !strings.Contains(action.Details, source.ID) {
		t.Fatal("missing atomic retry audit", action, err)
	}
	second, err := s.FinishRun(t.Context(), next.ID, autoRetryResult())
	if err != nil {
		t.Fatal(err)
	}
	if second.AutoRetriedBy != "" || second.AutoRetryOf != source.ID {
		t.Fatal("automatic retry retried twice", second)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM runs`).Scan(&count); err != nil || count != 2 {
		t.Fatal("wrong attempt count", count, err)
	}
}

func TestAutoRetryRefusesExecutedStepsReleasesAndStaleWork(t *testing.T) {
	for _, kind := range []string{"step-started", "step-passed", "failed-step", "tag", "release", "plan", "apply", "newer-terminal", "pending-delete", "publication"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Now()
			s := testStore(t, &now)
			repo := createRepo(t, s)
			spec := model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature/guard", SHA: strings.Repeat("a", 40), Actor: "pusher", Trigger: "branch"}
			if kind == "tag" {
				spec.RefKind = model.RefTag
			}
			if kind == "release" {
				spec.Release = true
			}
			if kind == "plan" || kind == "apply" {
				spec.Trigger = kind
			}
			// Promotion exclusion is covered with an ordinary durable source whose
			// trigger is changed before finishing; no passing promotion is fabricated.

			r, err := s.EnqueueRun(t.Context(), spec)
			if err != nil {
				t.Fatal(err)
			}
			result := autoRetryResult()
			switch kind {
			case "step-started", "step-passed":
				step := model.StepResult{RunID: r.ID, Burn: "ci", Step: "one", Status: model.StepPassed, DeclaredSize: "M"}
				if kind == "step-started" {
					step.Status = model.StepFailed
					step.StartedAt = &now
				}
				if _, err := s.PutStepResult(t.Context(), step); err != nil {
					t.Fatal(err)
				}
			case "failed-step":
				result.FailedBurn = "ci"
				result.FailedStep = "unit"
			case "newer-terminal":
				newer := autoRetrySource(t, s, repo.ID, spec.Ref)
				if _, err := s.FinishRun(t.Context(), newer.ID, model.RunResult{Status: model.RunPassed}); err != nil {
					t.Fatal(err)
				}
				// The older source was superseded; finishing it must not resurrect it.
				if _, err := s.FinishRun(t.Context(), r.ID, result); err == nil {
					t.Fatal("superseded source was finished/retried")
				}
				return
			case "pending-delete":
				if _, err := s.db.Exec(`INSERT INTO run_cancellations(run_id,job_name,superseded_by,reason,created_at) VALUES(?,'job','','owner_restart',?)`, r.ID, now.UnixNano()); err != nil {
					t.Fatal(err)
				}
			case "publication":
				if _, err := s.db.Exec(`INSERT INTO publications(id,repo_id,run_id,ref_kind,ref,result_sha,actor,created_at,updated_at) VALUES(?,?,?,'branch',?,?,?, ?,?)`, "pub-"+r.ID, repo.ID, r.ID, r.Ref, r.SHA, "oberth", now.UnixNano(), now.UnixNano()); err != nil {
					t.Fatal(err)
				}
			}
			failed, err := s.FinishRun(t.Context(), r.ID, result)
			if err != nil || failed.AutoRetriedBy != "" {
				t.Fatal("unsafe automatic retry", kind, failed, err)
			}
		})
	}
}

func TestAutoRetryLineageSurvivesRestartAndCancelledManualReplay(t *testing.T) {
	for _, mode := range []string{"restart", "manual-cancelled"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			s := testStore(t, &now)
			repo := createRepo(t, s)
			source := autoRetrySource(t, s, repo.ID, "feature/lineage")
			failed, err := s.FinishRun(t.Context(), source.ID, autoRetryResult())
			if err != nil {
				t.Fatal(err)
			}
			child, err := s.ClaimNextRun(t.Context())
			if err != nil || child.ID != failed.AutoRetriedBy {
				t.Fatal(err, child)
			}
			var next model.Run
			if mode == "restart" {
				next, err = s.RequeueStrandedRun(t.Context(), child.ID)
			} else {
				_, err = s.CancelRun(t.Context(), model.RunCancelRequest{RunID: child.ID, Actor: "owner", Reason: "stop"})
				if err == nil {
					// No Workflow was submitted; settle the exact deletion obligation first.
					err = s.CompleteSupersededRunCancellationWithoutJob(t.Context(), child.ID)
				}
				if err == nil {
					var rerun model.RunRerunResult
					rerun, err = s.RerunRun(t.Context(), model.RunRerunRequest{RunID: child.ID, Actor: "owner"})
					next = rerun.Enqueued.Run
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if next.AutoRetryOf != source.ID {
				t.Fatal("replay lost automatic retry lineage", next)
			}
			terminal, err := s.FinishRun(t.Context(), next.ID, autoRetryResult())
			if err != nil || terminal.AutoRetriedBy != "" {
				t.Fatal("replay reset automatic cap", terminal, err)
			}
		})
	}
}

func TestAutoRetryAuditFailureRollsBackFailureAndEnqueueTogether(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	source := autoRetrySource(t, s, repo.ID, "feature/atomic")
	if _, err := s.db.Exec(`CREATE TRIGGER fail_auto_retry BEFORE INSERT ON audit_actions WHEN NEW.action='run.retry.auto' BEGIN SELECT RAISE(ABORT,'test audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishRun(context.Background(), source.ID, autoRetryResult()); err == nil {
		t.Fatal("audit failure did not abort")
	}
	unchanged, err := s.Run(t.Context(), source.ID)
	if err != nil || unchanged.Status != model.RunQueued || unchanged.AutoRetriedBy != "" {
		t.Fatal("partial failure commit", unchanged, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM runs`).Scan(&count); err != nil || count != 1 {
		t.Fatal("orphan retry committed", count, err)
	}
}
