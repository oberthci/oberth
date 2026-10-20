package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestRerunRunRefusesWithoutQueueingAndRecordsLineage(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	const sha = "abababababababababababababababababababab"
	finish := func(spec model.RunSpec, result model.RunResult) model.Run {
		t.Helper()
		run, err := s.EnqueueRun(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimNextRun(ctx); err != nil {
			t.Fatal(err)
		}
		finished, err := s.FinishRun(ctx, run.ID, result)
		if err != nil {
			t.Fatal(err)
		}
		return finished
	}
	branch := model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature/x", SHA: sha, Actor: "agent@host", Trigger: "branch"}
	failed := finish(branch, model.RunResult{Status: model.RunFailed, Phase: "test", FailedBurn: "test", FailedStep: "unit"})
	const base = "bcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbc"
	enqueued, _, err := s.EnqueuePromotionRun(ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "promotion/main/abababababab", SHA: sha, Actor: "agent@host",
		Trigger: "promotion", TestedSHA: sha, BaseSHA: base,
	}, model.PromotionSpec{RepoID: repo.ID, SourceBranch: "feature/x", SourceSHA: sha, TargetRef: "main",
		PreviousSHA: base, ResultSHA: sha, Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	promotionRun, err := s.FinishRun(ctx, enqueued.ID, model.RunResult{Status: model.RunFailed, Phase: "test"})
	if err != nil {
		t.Fatal(err)
	}
	passed := finish(model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature/y", SHA: sha, Actor: "agent@host", Trigger: "branch"},
		model.RunResult{Status: model.RunPassed})

	for name, id := range map[string]string{"promotion CI": promotionRun.ID, "passed": passed.ID} {
		if _, err := s.RerunRun(ctx, model.RunRerunRequest{RunID: id, Actor: "agent@host"}); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("%s rerun error = %v, want ErrInvalidState", name, err)
		}
	}
	denied := errors.New("denied")
	if _, err := s.RerunRun(ctx, model.RunRerunRequest{RunID: failed.ID, Actor: "agent@host",
		Authorize: func(model.Run) (string, error) { return "", denied }}); !errors.Is(err, denied) {
		t.Fatalf("unauthorized rerun error = %v", err)
	}
	if _, err := s.RerunRun(ctx, model.RunRerunRequest{RunID: failed.ID, Actor: "agent@host", Reason: "two\nlines"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("multi-line reason error = %v, want ErrInvalid", err)
	}
	runs, err := s.ListRecentRuns(ctx, model.RunListFilter{RepoID: repo.ID, Limit: 10})
	if err != nil || len(runs) != 3 {
		t.Fatalf("runs after refused reruns = %d, %v; want 3", len(runs), err)
	}

	result, err := s.RerunRun(ctx, model.RunRerunRequest{RunID: failed.ID, Actor: "other@host", Reason: "flaky"})
	if err != nil {
		t.Fatal(err)
	}
	rerun := result.Enqueued.Run
	if rerun.Status != model.RunQueued || rerun.SHA != sha || rerun.Ref != failed.Ref || rerun.Actor != "other@host" {
		t.Fatalf("rerun = %#v", rerun)
	}
	if source, err := s.RunRerunOf(ctx, rerun.ID); err != nil || source != failed.ID {
		t.Fatalf("RunRerunOf(rerun) = %q, %v", source, err)
	}
	if source, err := s.RunRerunOf(ctx, failed.ID); err != nil || source != "" {
		t.Fatalf("RunRerunOf(original) = %q, %v", source, err)
	}
	if _, err := s.RerunRun(ctx, model.RunRerunRequest{RunID: failed.ID, Actor: "agent@host"}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("rerun beside an active run error = %v, want ErrInvalidState", err)
	}
	if _, err := s.VerifyAuditState(ctx); err != nil {
		t.Fatalf("audit chain after reruns: %v", err)
	}
}
