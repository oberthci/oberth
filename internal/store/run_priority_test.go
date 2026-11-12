package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

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
		t.Fatalf("aged standard lost promotion FIFO: %+v %v", next, err)
	}
	if got, err := s.ClaimNextRun(ctx); err != nil || got.ID != normal.ID {
		t.Fatalf("aged claim: %+v %v", got, err)
	}
	got, err := s.ClaimNextRun(ctx)
	if err != nil || got.ID != tag.ID {
		t.Fatalf("tag claim: %+v %v", got, err)
	}
	aged, err := s.Run(ctx, normal.ID)
	if err != nil {
		t.Fatal(err)
	}
	priority, reason := aged.EffectivePriority(now)
	if priority != model.PriorityPromotion || !strings.Contains(reason, "aged") || strings.Contains(reason, "highest") {
		t.Fatalf("aged display=%s/%s", priority, reason)
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
	got, err := s.BumpRunPriority(ctx, "admin", second.ID, model.PriorityHigh, "release blocker")
	if err != nil || got.Priority != model.PriorityHigh || got.PriorityReason != "admin bump: release blocker" {
		t.Fatalf("bump=%+v %v", got, err)
	}
	// Pattern changes cannot undo an explicit administrator bump.
	if _, err = s.SetQueuePriorityPatterns(ctx, "admin", []string{"feature/*"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetQueuePriorityPatterns(ctx, "admin", []string{}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.BumpRunPriority(ctx, "admin", second.ID, model.RunPriority("unknown"), "invalid class"); !errors.Is(err, ErrInvalid) {
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
	if _, err = reopened.BumpRunPriority(ctx, "admin", second.ID, model.PriorityHigh, "already started"); !errors.Is(err, ErrInvalidState) {
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
	if actor != "admin" || data["previous_priority"] != "standard" || data["priority"] != "high" || data["reason"] != "release blocker" {
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
	if p != model.PriorityPromotion || !copy.QueuedAt.Equal(run.QueuedAt) {
		t.Fatalf("restart reset aging clock: %+v", copy)
	}
	// A separately bumped item retains its explicit administrator reason.
	bumped, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/bump", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.BumpRunPriority(ctx, "admin", bumped.ID, model.PriorityHigh, "release blocker"); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimNextRun(ctx)
	if err != nil || claimed.ID != bumped.ID {
		t.Fatalf("base highest bump lost to aged normal: %+v %v", claimed, err)
	}
	aged, err := s.ClaimNextRun(ctx)
	if err != nil || aged.ID != copy.ID {
		t.Fatalf("aged original lost its FIFO place: %+v %v", aged, err)
	}
	retry, err := s.RequeueStrandedRun(ctx, bumped.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Priority != model.PriorityHigh || retry.PriorityReason != "admin bump: release blocker" {
		t.Fatalf("restart discarded admin bump: %+v", retry)
	}
}

// queueHead is the first queued row of a run list: the run admitted next.
func queueHead(runs []model.Run) model.Run {
	for i := range runs {
		if runs[i].QueuePosition > 0 {
			return runs[i]
		}
	}
	return model.Run{}
}

func TestQueuePriorityHighAgesWithoutLosingFIFOToNormals(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := t.Context()
	if _, err := s.SetQueuePriorityPatterns(ctx, "admin", []string{"rc/*"}); err != nil {
		t.Fatal(err)
	}
	high, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "rc/old", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	normal, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/newer", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	for i, want := range []string{high.ID, normal.ID} {
		flat, err := s.ListRecentRuns(ctx, model.RunListFilter{})
		if head := queueHead(flat); err != nil || head.ID != want || head.QueuePosition != 1 {
			t.Fatalf("aged queue view at %d: %+v %v", i, flat, err)
		}
		next, err := s.ClaimNextRun(ctx)
		if err != nil || next.ID != want {
			t.Fatalf("aged claim at %d: %+v %v", i, next, err)
		}
		p, _ := next.EffectivePriority(now)
		if p != model.PriorityPromotion {
			t.Fatalf("aged %s priority=%s", want, p)
		}
		if _, err := s.MarkRunAdmitted(ctx, next.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestQueuePriorityAgedStandardJoinsPromotionStreamFIFO(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := t.Context()
	old, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/old", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	origin := now
	now = origin.Add(45 * time.Minute)
	var tags []string
	for i := 0; i < 20; i++ {
		spec := testRunSpec(repo.ID, fmt.Sprintf("v1.%d", i), strings.Repeat("b", 40))
		spec.RefKind, spec.Trigger, spec.Release = model.RefTag, "tag", true
		tag, err := s.EnqueueRun(ctx, spec)
		want := model.PriorityStandard
		if i == 0 {
			want = model.PriorityPromotion
		}
		if err != nil || tag.Priority != want {
			t.Fatalf("ungranted release %d: %+v %v", i, tag, err)
		}
		tags = append(tags, tag.ID)
	}
	now = origin.Add(time.Hour - time.Nanosecond)
	next, err := s.NextQueuedRun(ctx)
	if err != nil || next.ID != tags[0] {
		t.Fatalf("release did not overtake younger-than-60m branch: %+v %v", next, err)
	}
	now = origin.Add(time.Hour)
	aged, err := s.ClaimNextRun(ctx)
	if err != nil || aged.ID != old.ID {
		t.Fatalf("aged branch lost promotion FIFO: %+v %v", aged, err)
	}
	if priority, _ := aged.EffectivePriority(now); priority != model.PriorityPromotion {
		t.Fatalf("aging lost at the 60-minute boundary: %s", priority)
	}
	for _, tagID := range tags {
		for _, read := range []func(context.Context) (model.Run, error){s.NextQueuedRun, s.ClaimNextRun} {
			next, err := read(ctx)
			if err != nil || next.ID != tagID {
				t.Fatalf("base highest tag stream lost order to aged normal: %+v %v", next, err)
			}
		}
		// Admission releases the automatic boost for the next waiting tag.
		if _, err := s.MarkRunAdmitted(ctx, tagID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FinishRun(ctx, tagID, model.RunResult{Status: model.RunPassed}); err != nil {
			t.Fatal(err)
		}
	}

}
