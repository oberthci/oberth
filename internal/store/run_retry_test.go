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
			if retried.Run.ID == source.ID || retried.Run.RepoID != repo.ID || retried.Run.Ref != source.Ref ||
				retried.Run.SHA != sha || retried.Run.TestedSHA != sha || retried.Run.Actor != "reviewer@host" ||
				retried.Run.Status != model.RunQueued || retried.Run.Trigger != "branch" || retried.Run.Credentialed {
				t.Fatalf("retry changed run identity or admission: %#v", retried.Run)
			}
			audit, err := s.LatestAuditActionByType(ctx, "run.retry", "run")
			if err != nil || audit.Actor != "reviewer@host" || audit.ResourceID != retried.Run.ID ||
				!strings.Contains(audit.Details, source.ID) {
				t.Fatalf("retry audit = %#v, %v", audit, err)
			}
			if _, err := s.RetryRun(ctx, source.ID, "reviewer@host"); !errors.Is(err, ErrInvalidState) {
				t.Fatalf("retry displaced active branch run: %v", err)
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
