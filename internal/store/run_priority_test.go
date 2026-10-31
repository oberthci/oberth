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
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestQueuePriorityClassificationAndFIFO(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()
	if _, err := s.SetSizeGrant(ctx, repo.ID, "admin", periapsis.L); err != nil {
		t.Fatal(err)
	}
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
	if _, err := s.SetSizeGrant(ctx, repo.ID, "admin", periapsis.L); err != nil {
		t.Fatal(err)
	}
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
	if _, err = s.db.ExecContext(ctx, `DROP TABLE IF EXISTS step_resource_usage; DROP INDEX IF EXISTS runs_latest_ref_idx; DROP TABLE run_priority_overrides; DROP TABLE queue_priority_patterns; DELETE FROM schema_migrations WHERE version>=34`); err != nil {
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
		if err != nil || flat[0].ID != want || flat[0].QueuePosition != 1 {
			t.Fatalf("aged queue view at %d: %+v %v", i, flat, err)
		}
		next, err := s.ClaimNextRun(ctx)
		if err != nil || next.ID != want {
			t.Fatalf("aged claim at %d: %+v %v", i, next, err)
		}
		p, _ := next.EffectivePriority(now)
		if p != model.PriorityHighest {
			t.Fatalf("aged %s priority=%s", want, p)
		}
		if _, err := s.MarkRunAdmitted(ctx, next.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestQueuePriorityTagsIndependentOfSizeGrantsAndBranchSignals(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := t.Context()
	other, err := s.CreateRepository(ctx, model.RepositorySpec{Name: "other", UpstreamID: repo.UpstreamID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	enqueue := func(repoID int64, ref string, kind model.RefKind, trigger string, release bool) model.Run {
		t.Helper()
		spec := testRunSpec(repoID, ref, strings.Repeat("a", 40))
		spec.RefKind, spec.Trigger, spec.Release = kind, trigger, release
		r, err := s.EnqueueRun(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		return r.Run
	}
	ordinary := enqueue(repo.ID, "ordinary", model.RefBranch, "push", false)
	tag := enqueue(repo.ID, "v1", model.RefTag, "tag", true)
	otherTag := enqueue(other.ID, "junk", model.RefTag, "tag", true)
	// Coherent tag runs receive priority without a grant. Branch-only signals do not.
	ungranted := []model.Run{tag, otherTag,
		enqueue(other.ID, "ref-only", model.RefTag, "push", false),
		enqueue(repo.ID, "trigger-only", model.RefBranch, "tag", false),
		enqueue(repo.ID, "release-trigger", model.RefBranch, "release", false),
		enqueue(repo.ID, "release-flag", model.RefBranch, "push", true),
	}
	for _, r := range ungranted {
		want := model.PriorityNormal
		if r.ID == tag.ID || r.ID == otherTag.ID {
			want = model.PriorityHighest
		}
		if r.Priority != want {
			t.Fatalf("ungranted tag/branch classification: %+v; want %s", r, want)
		}
	}
	if _, err = s.SetSizeGrant(ctx, repo.ID, "admin", periapsis.L); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id   string
		want model.RunPriority
	}{{tag.ID, model.PriorityHighest}, {otherTag.ID, model.PriorityHighest}, {ordinary.ID, model.PriorityNormal}} {
		r, err := s.Run(ctx, tc.id)
		if err != nil || r.Priority != tc.want {
			t.Fatalf("scoped grant: %+v %v", r, err)
		}
	}
	for _, candidate := range ungranted {
		if candidate.RefKind != model.RefBranch {
			continue
		}
		got, err := s.Run(ctx, candidate.ID)
		if err != nil || got.Priority != model.PriorityNormal {
			t.Fatalf("branch signals acquired release priority with a size grant: %+v %v", got, err)
		}
	}
	flat, err := s.ListRecentRuns(ctx, model.RunListFilter{})
	if err != nil || flat[0].ID != tag.ID {
		t.Fatalf("granted list order: %+v %v", flat, err)
	}
	claimed, err := s.ClaimNextRun(ctx)
	if err != nil || claimed.ID != tag.ID {
		t.Fatalf("granted claim: %+v %v", claimed, err)
	}
	if _, err = s.FinishRun(ctx, tag.ID, model.RunResult{Status: model.RunFailed, Error: "failed before publication"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetSizeGrant(ctx, repo.ID, "admin", ""); err != nil {
		t.Fatal(err)
	}
	before, err := s.CountRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RerunRun(ctx, model.RunRerunRequest{RunID: tag.ID, Actor: "admin", Force: true}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("tag rerun without authorization accepted: %v", err)
	}
	after, err := s.CountRuns(ctx)
	if err != nil || after.Total != before.Total {
		t.Fatalf("unauthorized rerun changed queue: before=%+v after=%+v err=%v", before, after, err)
	}
	authorized := false
	retry, err := s.RerunRun(ctx, model.RunRerunRequest{
		RunID: tag.ID, Actor: "admin", Reason: "replay release after size grant revocation", Force: true,
		ReplayAudit: &model.RunReplayAudit{Basis: "forced"},
		Authorize: func(current model.Run, _ model.RunReplayEvidence) (string, error) {
			if current.ID != tag.ID {
				t.Fatalf("authorized wrong run: %s", current.ID)
			}
			authorized = true
			return "explicit release replay authorization", nil
		},
	})
	if err != nil || !authorized || retry.Enqueued.Priority != model.PriorityNormal {
		t.Fatalf("rerun ignored automatic priority revocation: %+v %v", retry, err)
	}
	next, err := s.NextQueuedRun(ctx)
	if err != nil || next.ID != otherTag.ID {
		t.Fatalf("next genuine tag lost highest-class FIFO: %+v %v", next, err)
	}
	for _, action := range []string{"size.grant", "size.revoke"} {
		var actor, details string
		err := s.db.QueryRowContext(ctx, `SELECT actor,details FROM audit_actions WHERE action=? AND resource_id=?`, action, repo.ID).Scan(&actor, &details)
		var data map[string]any
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal([]byte(details), &data); err != nil {
			t.Fatal(err)
		}
		_, tagFlag := data["tag_priority"]
		_, previousTagFlag := data["previous_tag_priority"]
		if actor != "admin" || tagFlag || previousTagFlag || data["automatic_tag_priority_enabled"] != (action == "size.grant") || data["previous_automatic_tag_priority_enabled"] != true {
			t.Fatalf("size audit has incorrect automatic priority state: %s by %s: %s", action, actor, details)
		}
	}
	// Trusted plan/apply have cancellation guards, but no automatic urgency.
	for _, trigger := range []string{"plan", "apply"} {
		r := enqueue(repo.ID, trigger, model.RefBranch, trigger, false)
		if r.Priority != model.PriorityNormal {
			t.Fatalf("trusted %s acquired automatic priority: %+v", trigger, r)
		}
	}
}

func TestQueuePriorityReleaseStreamKeepsSixtyMinuteFairnessBound(t *testing.T) {
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
	var firstTag string
	for i := 0; i < 20; i++ {
		spec := testRunSpec(repo.ID, fmt.Sprintf("v1.%d", i), strings.Repeat("b", 40))
		spec.RefKind, spec.Trigger, spec.Release = model.RefTag, "tag", true
		tag, err := s.EnqueueRun(ctx, spec)
		want := model.PriorityNormal
		if i == 0 {
			want = model.PriorityHighest
		}
		if err != nil || tag.Priority != want {
			t.Fatalf("ungranted release %d: %+v %v", i, tag, err)
		}
		if i == 0 {
			firstTag = tag.ID
		}
	}
	now = origin.Add(time.Hour - time.Nanosecond)
	next, err := s.NextQueuedRun(ctx)
	if err != nil || next.ID != firstTag {
		t.Fatalf("release did not overtake younger-than-60m branch: %+v %v", next, err)
	}
	now = origin.Add(time.Hour)
	for _, read := range []func(context.Context) (model.Run, error){s.NextQueuedRun, s.ClaimNextRun} {
		next, err := read(ctx)
		if err != nil || next.ID != old.ID {
			t.Fatalf("tag stream starved old branch at boundary: %+v %v", next, err)
		}
	}
}

func TestQueuePriorityOneWaitingTagPerRepoAndAdminOptOut(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	ctx := t.Context()
	repo := createRepo(t, s)
	other, err := s.CreateRepository(ctx, model.RepositorySpec{Name: "second", UpstreamID: repo.UpstreamID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	enqueue := func(repoID int64, ref string) model.Run {
		t.Helper()
		spec := testRunSpec(repoID, ref, strings.Repeat("a", 40))
		spec.RefKind, spec.Trigger, spec.Release = model.RefTag, "tag", true
		r, err := s.EnqueueRun(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		return r.Run
	}
	first := enqueue(repo.ID, "junk-1")
	second := enqueue(repo.ID, "junk-2")
	third := enqueue(repo.ID, "junk-3")
	otherTag := enqueue(other.ID, "junk-1")
	check := func(id string, want model.RunPriority, reason string) {
		t.Helper()
		r, err := s.Run(ctx, id)
		if err != nil || r.Priority != want || !strings.Contains(r.PriorityReason, reason) {
			t.Fatalf("priority=%+v %v; want %s/%s", r, err, want, reason)
		}
	}
	check(first.ID, model.PriorityHighest, "oldest waiting")
	check(second.ID, model.PriorityNormal, "earlier tag")
	check(third.ID, model.PriorityNormal, "earlier tag")
	check(otherTag.ID, model.PriorityHighest, "oldest waiting")
	claimed, err := s.ClaimNextRun(ctx)
	if err != nil || claimed.ID != first.ID || claimed.StartedAt != nil {
		t.Fatalf("claim=%+v %v", claimed, err)
	}
	// Claiming is not resource admission: don't grant a second automatic boost.
	check(second.ID, model.PriorityNormal, "earlier tag")
	if _, err := s.MarkRunAdmitted(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	check(second.ID, model.PriorityHighest, "oldest waiting")
	check(third.ID, model.PriorityNormal, "earlier tag")
	if _, err := s.FinishRun(ctx, second.ID, model.RunResult{Status: model.RunFailed}); err != nil {
		t.Fatal(err)
	}
	check(third.ID, model.PriorityHighest, "oldest waiting")
	// Admin opt-out works on a never-granted repository, survives future tags
	// and does not disable other repos, promotions, explicit bumps or aging.
	grant, err := s.SetSizeGrant(ctx, repo.ID, "admin", "")
	if err != nil || grant.RevokedAt == nil {
		t.Fatalf("revoke never granted=%+v %v", grant, err)
	}
	check(third.ID, model.PriorityNormal, "disabled by size_revoke")
	newTag := enqueue(repo.ID, "junk-4")
	check(newTag.ID, model.PriorityNormal, "disabled by size_revoke")
	check(otherTag.ID, model.PriorityHighest, "oldest waiting")
	if _, err := s.BumpRunPriority(ctx, "admin", third.ID, model.PriorityHigh, "intentional expedite"); err != nil {
		t.Fatal(err)
	}
	check(third.ID, model.PriorityHigh, "admin bump")
	spec := testRunSpec(repo.ID, "promotion/main/1", strings.Repeat("b", 40))
	spec.Trigger = "promotion"
	promotion, err := s.EnqueueRun(ctx, spec)
	if err != nil || promotion.Priority != model.PriorityHighest {
		t.Fatalf("promotion disabled=%+v %v", promotion, err)
	}
	// L is sufficient to opt back in; the per-repo cap still applies.
	if _, err := s.SetSizeGrant(ctx, repo.ID, "admin", periapsis.L); err != nil {
		t.Fatal(err)
	}
	check(third.ID, model.PriorityHighest, "oldest waiting")
	if bumped, err := s.Run(ctx, third.ID); err != nil || bumped.PriorityOverride != model.PriorityHigh || bumped.PriorityOverrideReason != "intentional expedite" {
		t.Fatalf("automatic boost discarded explicit fallback: %+v %v", bumped, err)
	}
	check(newTag.ID, model.PriorityNormal, "earlier tag")
	// Exercise both run-list CTE and per-repo subquery correlations.
	for _, list := range []func() ([]model.Run, error){
		func() ([]model.Run, error) { return s.ListRecentRuns(ctx, model.RunListFilter{}) },
		func() ([]model.Run, error) { return s.ListLatestRunsPerRepo(ctx, 12) },
	} {
		runs, err := list()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range runs {
			if r.ID == newTag.ID && r.Priority != model.PriorityNormal {
				t.Fatalf("list bypassed per-repo cap: %+v", r)
			}
		}
	}
}
