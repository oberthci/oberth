package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestQueuePriorityClassificationAndFIFO(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()
	enqueue := func(ref string, kind model.RefKind, trigger string) model.Run {
		t.Helper()
		spec := testRunSpec(repo.ID, ref, strings.Repeat("a", 40))
		spec.RefKind = kind
		spec.Trigger = trigger
		result, err := s.EnqueueRun(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		return result.Run
	}
	normal := enqueue("feature/first", model.RefBranch, "push")
	rc := enqueue("rc/v1", model.RefBranch, "push")
	release := enqueue("v1", model.RefTag, "tag")
	promotion := enqueue("promotion/main/1", model.RefBranch, "promotion")
	normal2 := enqueue("feature/last", model.RefBranch, "push")
	// A repository can name its branch release/* but has no priority until
	// the installation administrator configures it.
	before, err := s.Run(ctx, rc.ID)
	if err != nil || before.Priority != model.PriorityNormal {
		t.Fatalf("repo self-classified: %+v, %v", before, err)
	}
	if _, err = s.SetQueuePriorityPatterns(ctx, "admin", []string{"rc/*", "release/*"}); err != nil {
		t.Fatal(err)
	}
	rc, err = s.Run(ctx, rc.ID)
	if err != nil || rc.Priority != model.PriorityHigh || !strings.Contains(rc.PriorityReason, "rc/*") {
		t.Fatalf("RC classification: %+v %v", rc, err)
	}
	want := []string{release.ID, promotion.ID, rc.ID, normal.ID, normal2.ID}
	active, err := s.ListActiveRuns(ctx, model.RunListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	flat, err := s.ListRecentRuns(ctx, model.RunListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range want {
		if active[i].ID != id || flat[i].ID != id || active[i].QueuePosition != int64(i+1) || flat[i].QueuePosition != int64(i+1) {
			t.Fatalf("queue views disagree at %d: active=%+v flat=%+v", i, active[i], flat[i])
		}
		if pos, err := s.QueuedRunPosition(ctx, id); err != nil || pos != int64(i+1) {
			t.Fatalf("position %s=%d, %v", id, pos, err)
		}
	}
	for _, id := range want {
		peek, err := s.NextQueuedRun(ctx)
		if err != nil || peek.ID != id {
			t.Fatalf("peek %s: %+v %v", id, peek, err)
		}
		got, err := s.ClaimNextRun(ctx)
		if err != nil || got.ID != id {
			t.Fatalf("claim %s: %+v %v", id, got, err)
		}
	}
}

func TestQueuePriorityStarvationAtBoundary(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()
	normal, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/old", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour - time.Nanosecond)
	tagSpec := testRunSpec(repo.ID, "v1", strings.Repeat("b", 40))
	tagSpec.RefKind = model.RefTag
	tagSpec.Trigger = "tag"
	tag, err := s.EnqueueRun(ctx, tagSpec)
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.NextQueuedRun(ctx)
	if err != nil || next.ID != tag.ID {
		t.Fatalf("early starvation boost: %+v %v", next, err)
	}
	now = now.Add(time.Nanosecond)
	next, err = s.NextQueuedRun(ctx)
	if err != nil || next.ID != normal.ID {
		t.Fatalf("starved run behind release: %+v %v", next, err)
	}
	got, err := s.ClaimNextRun(ctx)
	if err != nil || got.ID != normal.ID {
		t.Fatalf("starved claim: %+v %v", got, err)
	}
	priority, reason := got.EffectivePriority(now)
	if priority != model.PriorityHighest || !strings.Contains(reason, "starvation") {
		t.Fatalf("starved display=%s/%s", priority, reason)
	}
}

func TestQueuePriorityBumpAuditedAndSurvivesReopen(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()
	first, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/a", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/b", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.BumpRunPriority(ctx, "admin", second.ID, model.PriorityHighest, "release blocker")
	if err != nil || got.Priority != model.PriorityHighest || got.PriorityReason != "admin bump: release blocker" {
		t.Fatalf("bump=%+v %v", got, err)
	}
	// Pattern changes cannot undo an explicit administrator bump.
	if _, err = s.SetQueuePriorityPatterns(ctx, "admin", []string{"feature/*"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetQueuePriorityPatterns(ctx, "admin", []string{}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.BumpRunPriority(ctx, "admin", second.ID, model.PriorityHigh, "lower"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("lower=%v", err)
	}
	reopened, err := Open(ctx, s.livePath, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	next, err := reopened.ClaimNextRun(ctx)
	if err != nil || next.ID != second.ID {
		t.Fatalf("persisted priority=%+v %v", next, err)
	}
	if _, err = reopened.MarkRunAdmitted(ctx, next.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.BumpRunPriority(ctx, "admin", second.ID, model.PriorityHighest, "already started"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("bumped admitted work=%v", err)
	}
	var details, actor string
	if err = s.db.QueryRowContext(ctx, `SELECT details,actor FROM audit_actions WHERE action='run.priority' AND resource_id=?`, second.ID).Scan(&details, &actor); err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err = json.Unmarshal([]byte(details), &data); err != nil {
		t.Fatal(err)
	}
	if actor != "admin" || data["previous_priority"] != "normal" || data["priority"] != "highest" || data["reason"] != "release blocker" {
		t.Fatalf("audit=%s actor=%s", details, actor)
	}
	next, err = reopened.ClaimNextRun(ctx)
	if err != nil || next.ID != first.ID {
		t.Fatalf("ordinary run lost: %+v %v", next, err)
	}
}

func TestQueuePriorityPatternsValidation(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	ctx := context.Background()
	for _, patterns := range [][]string{{""}, {"rc/[abc]"}, {"rc/*", "rc/*"}, {"rc/\n*"}, {strings.Repeat("x", 257)}} {
		if _, err := s.SetQueuePriorityPatterns(ctx, "admin", patterns); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted patterns %q: %v", patterns, err)
		}
	}
}

func TestQueuePriorityRestartRefireRetainsWaitAndBump(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()
	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/old", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	copy, err := s.RequeueStrandedRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := copy.EffectivePriority(now)
	if p != model.PriorityHighest || !copy.QueuedAt.Equal(run.QueuedAt) {
		t.Fatalf("restart reset starvation clock: %+v", copy)
	}
	// A separately bumped item retains its explicit administrator reason.
	bumped, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/bump", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.BumpRunPriority(ctx, "admin", bumped.ID, model.PriorityHighest, "release blocker"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	} // aged original is FIFO first
	claimed, err := s.ClaimNextRun(ctx)
	if err != nil || claimed.ID != bumped.ID {
		t.Fatalf("bump claim=%+v %v", claimed, err)
	}
	retry, err := s.RequeueStrandedRun(ctx, bumped.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Priority != model.PriorityHighest || retry.PriorityReason != "admin bump: release blocker" {
		t.Fatalf("restart discarded admin bump: %+v", retry)
	}
}

func TestQueuePriorityMigrationFromV33ClassifiesExistingWork(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()
	normal, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/a", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	spec := testRunSpec(repo.ID, "v1", strings.Repeat("b", 40))
	spec.RefKind = model.RefTag
	spec.Trigger = "tag"
	tag, err := s.EnqueueRun(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `DROP TABLE run_priority_overrides; DROP TABLE queue_priority_patterns; DELETE FROM schema_migrations WHERE version>=34`); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(ctx, s.livePath, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = upgraded.Close() }()
	patterns, err := upgraded.QueuePriorityPatterns(ctx)
	if err != nil || len(patterns) != 0 {
		t.Fatalf("migration invented RC patterns: %q %v", patterns, err)
	}
	for _, id := range []string{tag.ID, normal.ID} {
		r, err := upgraded.ClaimNextRun(ctx)
		if err != nil || r.ID != id {
			t.Fatalf("migrated queue order: %+v %v", r, err)
		}
	}
}
