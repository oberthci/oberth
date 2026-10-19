package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestRetryRunKeepsSourceAndAuditsActor(t *testing.T) {
	for _, result := range []model.RunResult{
		{Status: model.RunInterrupted, Phase: "interrupted"},
		{Status: model.RunFailed, Phase: "infrastructure", Error: "checkout unavailable"},
		{Status: model.RunFailed, Phase: "job", Error: "workflow deadline"},
	} {
		t.Run(string(result.Status), func(t *testing.T) {
			now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
			s := testStore(t, &now)
			ctx := context.Background()
			repo := createRepo(t, s)
			sha := strings.Repeat("a", 40)
			source, err := s.EnqueueRun(ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch,
				Ref: "feature/retry", SHA: sha, Actor: "pusher", Trigger: "branch"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.FinishRun(ctx, source.ID, result); err != nil {
				t.Fatal(err)
			}
			retried, err := s.RetryRun(ctx, source.ID, "reviewer@host")
			if err != nil {
				t.Fatal(err)
			}
			if retried.ID == source.ID || retried.RepoID != repo.ID || retried.Ref != source.Ref ||
				retried.SHA != sha || retried.TestedSHA != sha || retried.Actor != "reviewer@host" ||
				retried.Status != model.RunQueued || retried.Trigger != "branch" || retried.Credentialed {
				t.Fatalf("retry changed run identity or admission: %#v", retried.Run)
			}
			audit, err := s.LatestAuditActionByType(ctx, "run.retry", "run")
			if err != nil || audit.Actor != "reviewer@host" || audit.ResourceID != retried.ID ||
				!strings.Contains(audit.Details, source.ID) {
				t.Fatalf("retry audit = %#v, %v", audit, err)
			}
			if _, err := s.RetryRun(ctx, source.ID, "reviewer@host"); !errors.Is(err, ErrInvalidState) {
				t.Fatalf("retry displaced active branch run: %v", err)
			}
			if _, err := s.FinishRun(ctx, retried.ID, model.RunResult{Status: model.RunPassed}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.RetryRun(ctx, source.ID, "reviewer@host"); !errors.Is(err, ErrInvalidState) {
				t.Fatalf("source retried again after newer run finished: %v", err)
			}
		})
	}
}

func TestRetryRunRefusesTestFailuresAndReleaseRuns(t *testing.T) {
	for _, tc := range []struct {
		name   string
		spec   model.RunSpec
		result model.RunResult
	}{
		{name: "test failure", result: model.RunResult{Status: model.RunFailed, Phase: "test", FailedBurn: "test", FailedStep: "unit"}},
		{name: "fake infrastructure phase", result: model.RunResult{Status: model.RunFailed, Phase: "infrastructure", FailedBurn: "test", FailedStep: "unit"}},
		{name: "job phase with failed step", result: model.RunResult{Status: model.RunFailed, Phase: "job", FailedBurn: "test", FailedStep: "unit"}},
		{name: "release", spec: model.RunSpec{RefKind: model.RefTag, Ref: "v1", Release: true, Trigger: "release"}, result: model.RunResult{Status: model.RunInterrupted}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
			s := testStore(t, &now)
			ctx := context.Background()
			repo := createRepo(t, s)
			spec := tc.spec
			spec.RepoID, spec.SHA, spec.Actor = repo.ID, strings.Repeat("a", 40), "pusher"
			if spec.RefKind == "" {
				spec.RefKind, spec.Ref, spec.Trigger = model.RefBranch, "feature/retry", "branch"
			}
			source, err := s.EnqueueRun(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.FinishRun(ctx, source.ID, tc.result); err != nil {
				t.Fatal(err)
			}
			if _, err := s.RetryRun(ctx, source.ID, "reviewer@host"); !errors.Is(err, ErrInvalidState) {
				t.Fatalf("ineligible run retried: %v", err)
			}
			counts, err := s.CountRuns(ctx)
			if err != nil || counts.Total != 1 {
				t.Fatalf("ineligible retry enqueued a run: %#v, %v", counts, err)
			}
		})
	}
}

func TestRetryRunRefusesOlderBranchRunAfterNewerRunFinishes(t *testing.T) {
	for _, newerSHA := range []string{strings.Repeat("b", 40), strings.Repeat("a", 40)} {
		t.Run(newerSHA[:1], func(t *testing.T) {
			now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
			s := testStore(t, &now)
			ctx := context.Background()
			repo := createRepo(t, s)
			old, err := s.EnqueueRun(ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch,
				Ref: "feature/retry", SHA: strings.Repeat("a", 40), Actor: "pusher", Trigger: "branch"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.FinishRun(ctx, old.ID, model.RunResult{Status: model.RunInterrupted}); err != nil {
				t.Fatal(err)
			}
			newer, err := s.EnqueueRun(ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch,
				Ref: old.Ref, SHA: newerSHA, Actor: "pusher", Trigger: "branch"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.FinishRun(ctx, newer.ID, model.RunResult{Status: model.RunPassed}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.RetryRun(ctx, old.ID, "reviewer@host"); !errors.Is(err, ErrInvalidState) {
				t.Fatalf("older run retry = %v", err)
			}
			latest, err := s.ResolveRunByRefKind(ctx, repo.ID, old.Ref, model.RefBranch)
			if err != nil || latest.ID != newer.ID {
				t.Fatalf("latest run changed after refused retry: %+v, %v", latest, err)
			}
			counts, err := s.CountRuns(ctx)
			if err != nil || counts.Total != 2 {
				t.Fatalf("refused retry enqueued a run: %+v, %v", counts, err)
			}
		})
	}
}

func TestRetryRunRefusesSupersededSource(t *testing.T) {
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()
	repo := createRepo(t, s)
	source, err := s.EnqueueRun(ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch,
		Ref: "feature/retry", SHA: strings.Repeat("a", 40), Actor: "pusher", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	replacement, err := s.EnqueueRun(ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch,
		Ref: source.Ref, SHA: strings.Repeat("b", 40), Actor: "pusher", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.Run(ctx, source.ID)
	if err != nil || old.Status != model.RunInterrupted || old.SupersededBy != replacement.ID {
		t.Fatalf("superseded source = %+v, %v", old, err)
	}
	if _, err := s.FinishRun(ctx, replacement.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RetryRun(ctx, old.ID, "reviewer@host"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("superseded source retried: %v", err)
	}
}
