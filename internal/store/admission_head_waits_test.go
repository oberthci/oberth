package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestAdmissionHeadWaitKeepsTheFirstOriginAndExpires(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/acme"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := database.CreateRepository(ctx, model.RepositorySpec{Name: "ebpf", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	if _, found, err := database.AdmissionHeadWait(ctx, repository.ID, model.RefBranch, "main", sha); err != nil || found {
		t.Fatalf("missing origin = %v, %v", found, err)
	}
	first := now.Add(-5 * time.Minute)
	for _, since := range []time.Time{first, now} {
		if err := database.RecordAdmissionHeadWait(ctx, repository.ID, model.RefBranch, "main", sha, since); err != nil {
			t.Fatal(err)
		}
	}
	if since, found, err := database.AdmissionHeadWait(ctx, repository.ID, model.RefBranch, "main", strings.ToUpper(sha)); err != nil || !found || !since.Equal(first) {
		t.Fatalf("origin = %s, %v, %v; want the first %s", since, found, err, first)
	}
	now = now.Add(25 * time.Hour)
	if _, found, err := database.AdmissionHeadWait(ctx, repository.ID, model.RefBranch, "main", sha); err != nil || found {
		t.Fatalf("a day-old origin was inherited: %v, %v", found, err)
	}
	if err := database.RecordAdmissionHeadWait(ctx, repository.ID, model.RefBranch, "main", sha, now); err != nil {
		t.Fatal(err)
	}
	if err := database.ClearAdmissionHeadWait(ctx, repository.ID, model.RefBranch, "main", sha); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := database.AdmissionHeadWait(ctx, repository.ID, model.RefBranch, "main", sha); found {
		t.Fatal("a cleared origin is still found")
	}
}

func TestRunDurationEstimateIsTheMedianOfRecentPassedRuns(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/acme"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := database.CreateRepository(ctx, model.RepositorySpec{Name: "cli", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	finish := func(index int, minutes int, status model.RunStatus) {
		t.Helper()
		run, err := database.EnqueueRun(ctx, model.RunSpec{RepoID: repository.ID, RefKind: model.RefBranch, Ref: "feature/" + string(rune('a'+index)),
			SHA: strings.Repeat(string(rune('a'+index%6)), 40), Actor: "agent@host", Trigger: "branch"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.ClaimNextRun(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := database.SetRunJobName(ctx, run.ID, "job-"+run.ID[:8]); err != nil {
			t.Fatal(err)
		}
		if _, err := database.MarkRunSubmitted(ctx, run.ID); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Duration(minutes) * time.Minute)
		if _, err := database.FinishRun(ctx, run.ID, model.RunResult{Status: status, Phase: string(status)}); err != nil {
			t.Fatal(err)
		}
	}
	finish(0, 4, model.RunPassed)
	finish(1, 10, model.RunPassed)
	if _, found, err := database.RunDurationEstimate(ctx, repository.ID, "branch"); err != nil || found {
		t.Fatalf("two samples gave an estimate: %v, %v", found, err)
	}
	finish(2, 6, model.RunPassed)
	finish(3, 90, model.RunFailed) // failures do not count
	estimate, found, err := database.RunDurationEstimate(ctx, repository.ID, "branch")
	if err != nil || !found || estimate != 6*time.Minute {
		t.Fatalf("estimate = %s, %v, %v; want the median 6m", estimate, found, err)
	}
	if _, found, _ := database.RunDurationEstimate(ctx, repository.ID, "promotion"); found {
		t.Fatal("branch history estimated a promotion")
	}
	if _, found, _ := database.RunDurationEstimate(ctx, repository.ID, "release"); found {
		t.Fatal("branch history estimated a release")
	}
	if _, _, err := database.RunDurationEstimate(ctx, repository.ID, "tag"); err == nil {
		t.Fatal("an unknown class was accepted")
	}
}

func TestSweepAdmissionHeadWaitsDropsOldRows(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/acme"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := database.CreateRepository(ctx, model.RepositorySpec{Name: "ebpf", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	old, fresh := strings.Repeat("a", 40), strings.Repeat("b", 40)
	if err := database.RecordAdmissionHeadWait(ctx, repository.ID, model.RefBranch, "promotion/main/a", old, now.Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := database.RecordAdmissionHeadWait(ctx, repository.ID, model.RefTag, "v1", fresh, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := database.SweepAdmissionHeadWaits(ctx); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM admission_head_waits`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows after sweep = %d, %v; want only the fresh one", rows, err)
	}
}
