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
		Authorize: func(model.Run, model.RunReplayEvidence) (string, error) { return "", denied }}); !errors.Is(err, denied) {
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

// The lineage is loaded once and kept current: a rerun made after the first
// lookup is visible at once, and a new Store reads every link back from the
// audit chain.
func TestRunRerunOfLineageCacheStaysCurrent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	path := t.TempDir() + "/oberth.db"
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	rerunOnce := func(ref, sha string) (string, string) {
		t.Helper()
		run, err := s.EnqueueRun(ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: ref, SHA: sha, Actor: "agent@host", Trigger: "branch"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimNextRun(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FinishRun(ctx, run.ID, model.RunResult{Status: model.RunFailed, Phase: "test", FailedStep: "unit"}); err != nil {
			t.Fatal(err)
		}
		result, err := s.RerunRun(ctx, model.RunRerunRequest{RunID: run.ID, Actor: "agent@host"})
		if err != nil {
			t.Fatal(err)
		}
		return result.Enqueued.ID, run.ID
	}
	before, beforeSource := rerunOnce("feature/a", "abababababababababababababababababababab")
	if source, err := s.RunRerunOf(ctx, before); err != nil || source != beforeSource {
		t.Fatalf("lineage loaded after the rerun = %q, %v", source, err)
	}
	after, afterSource := rerunOnce("feature/b", "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd")
	if source, err := s.RunRerunOf(ctx, after); err != nil || source != afterSource {
		t.Fatalf("lineage of a rerun after the first load = %q, %v", source, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	for run, want := range map[string]string{before: beforeSource, after: afterSource, beforeSource: ""} {
		if source, err := reopened.RunRerunOf(ctx, run); err != nil || source != want {
			t.Fatalf("reopened RunRerunOf(%s) = %q, %v; want %q", run, source, err, want)
		}
	}
}
