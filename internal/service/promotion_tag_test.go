package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitssh"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/pushretry"
)

// taggingGit records annotated tag pushes over the fixed upstream head.
type taggingGit struct {
	*upstreamGit
	pushes      []string
	err         error
	contains    bool
	commit      string
	published   bool
	unreachable bool
	beforePush  func()
}

func (g *taggingGit) PublishAnnotatedTag(_ context.Context, _, tag, commit, _ string) (string, error) {
	g.commit = commit
	if g.beforePush != nil {
		g.beforePush()
	}
	g.pushes = append(g.pushes, tag+"@"+commit)
	if g.err != nil {
		return "", g.err
	}
	g.published = true
	return strings.Repeat("e", 40), nil
}

func newTagPromotionFixture(t *testing.T) (*pruneFixture, *taggingGit) {
	t.Helper()
	f, upstream := newPrunePromotionFixture(t)
	git := &taggingGit{upstreamGit: upstream}
	pushes, err := NewPushIngestor(PushConfig{Repositories: f.store, Discoverer: stubRepositoryDiscoverer{}, Git: git, Receives: f.store, Branches: f.scheduler, Releases: f.scheduler, Auditor: f.store})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewAPI(APIConfig{
		Tags: pushes, Runs: f.store, History: f.store, Repositories: f.store, Issues: f.store, Promotions: f.store, PromotionRuns: f.store,
		Enqueues: fixtureEnqueues{f.scheduler}, Git: git, Logs: f.logs, Auditor: f.store,
		BranchPrune: f.cache, BranchDeletions: f.store, DeletionQueued: func() { f.queued++ },
		AgentEvents: f.store,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.service = service
	return f, git
}

func (f *pruneFixture) taggedPromotion(t *testing.T, status model.PromotionStatus, result string) (model.Promotion, model.PromotionTag) {
	t.Helper()
	promotion, err := f.store.AppendPromotion(t.Context(), model.PromotionSpec{RepoID: f.repo.ID, SourceBranch: "feature/tag", SourceSHA: f.initial, TargetRef: "main", Actor: "dev@host"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.RequestPromotionTag(t.Context(), promotion.ID, "dev@host", "deploy/1", "apply it"); err != nil {
		t.Fatal(err)
	}
	request, err := f.store.PromotionTag(t.Context(), promotion.ID)
	if err != nil {
		t.Fatal(err)
	}
	promotion.Status, promotion.ResultSHA = status, result
	return promotion, request
}

// tag_after pushes only after a passed promotion whose result is the
// upstream target, retries transient failures on the publication schedule
// and records every other outcome as failed with a reason (#1524).
func TestPromotionTagAfterOnlyAfterVerifiedPromotion(t *testing.T) {
	f, git := newTagPromotionFixture(t)
	now := time.Now()
	settle := func(promotion model.Promotion, request model.PromotionTag, at time.Time) model.PromotionTag {
		return f.service.settlePromotionTag(t.Context(), f.store, promotion, request, at)
	}
	failed, request := f.taggedPromotion(t, model.PromotionFailed, "")
	if got := settle(failed, request, now); got.Status != model.PromotionTagFailed || !strings.Contains(got.Reason, "promotion failed") || len(git.pushes) != 0 {
		t.Fatalf("failed promotion = %+v", got)
	}
	git.head = f.unmerged
	moved, request := f.taggedPromotion(t, model.PromotionPassed, f.initial)
	if got := settle(moved, request, now); got.Status != model.PromotionTagFailed || !strings.Contains(got.Reason, "not the promotion result") || len(git.pushes) != 0 {
		t.Fatalf("target differs = %+v", got)
	}

	git.head = f.initial
	git.err = fmt.Errorf("push: %w", gitssh.ErrTransportUnavailable)
	flaky, request := f.taggedPromotion(t, model.PromotionPassed, f.initial)
	at := now
	for attempt := 1; attempt < pushretry.MaxAttempts; attempt++ {
		request = settle(flaky, request, at)
		if request.Status != model.PromotionTagPending || request.Attempts != attempt || request.NextRetryAt == nil {
			t.Fatalf("attempt %d = %+v", attempt, request)
		}
		if early := settle(flaky, request, at); early.Attempts != attempt {
			t.Fatalf("retried before next_retry_at: %+v", early)
		}
		at = request.NextRetryAt.Add(time.Second)
	}
	if got := settle(flaky, request, at); got.Status != model.PromotionTagFailed || !strings.Contains(got.Reason, "gave up after") || got.Attempts != pushretry.MaxAttempts {
		t.Fatalf("exhausted retries = %+v", got)
	}

	git.err = fmt.Errorf("%w: upstream refs/tags/deploy/1", gitcache.ErrTagExists)
	taken, request := f.taggedPromotion(t, model.PromotionPassed, f.initial)
	if got := settle(taken, request, now); got.Status != model.PromotionTagFailed || !strings.Contains(got.Reason, "never moved") {
		t.Fatalf("existing tag = %+v", got)
	}

	git.err, git.pushes = nil, nil
	passed, request := f.taggedPromotion(t, model.PromotionPassed, f.initial)
	got := settle(passed, request, now)
	if got.Status != model.PromotionTagPushed || got.ObjectSHA != strings.Repeat("e", 40) || len(git.pushes) != 1 || git.pushes[0] != "deploy/1@"+f.initial {
		t.Fatalf("pushed = %+v %v", got, git.pushes)
	}
	actions, _, err := f.store.ListAuditActions(t.Context(), model.AuditListFilter{ActionPrefix: "promotion.tag_after.pushed", Limit: 20})
	if err != nil || len(actions) != 1 {
		t.Fatalf("audit = %d %v", len(actions), err)
	}
}

// The recorder settles the request and promotion.finished carries it.
func TestPromotionTagAfterInPromotionFinished(t *testing.T) {
	f, git := newTagPromotionFixture(t)
	git.head = f.initial
	promotion, _ := f.taggedPromotion(t, model.PromotionPassed, f.initial)
	f.service.agentEvents.started = time.Now()
	f.service.agentEvents.observePromotion(t.Context(), promotion)
	events, err := f.store.AgentEvents(t.Context(), model.AgentEventFilter{Kinds: []string{model.AgentEventPromotionFinished}})
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %+v %v", events, err)
	}
	tag, _ := events[0].Data["tag_after"].(map[string]any)
	if tag["status"] != model.PromotionTagPushed || tag["name"] != "deploy/1" || !strings.Contains(events[0].Summary, "tag deploy/1 pushed") {
		t.Fatalf("promotion.finished = %+v", events[0])
	}
}

// promote refuses a malformed tag_after before anything is admitted.
func TestPromoteTagAfterRefusals(t *testing.T) {
	f, _ := newTagPromotionFixture(t)
	for _, tag := range []map[string]any{{"name": "bad..name", "message": "m"}, {"name": "refs/tags/x", "message": "m"}, {"name": "ok", "message": " "}, {"name": "ok", "message": strings.Repeat("m", 4097)}} {
		raw, _ := json.Marshal(map[string]any{"repo": f.repo.Name, "branch": "main", "sha": f.initial, "tag_after": tag})
		if _, err := f.service.CallTool(t.Context(), pruneAdmin, "promote", raw); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("tag_after %v = %v", tag, err)
		}
	}
}

// tag_after creates an upstream ref no promotion grant covers: admins only.
func TestPromoteTagAfterRequiresAdmin(t *testing.T) {
	f, git := newTagPromotionFixture(t)
	raw, _ := json.Marshal(map[string]any{"repo": f.repo.Name, "branch": "main", "sha": f.initial, "tag_after": map[string]any{"name": "deploy/1", "message": "m"}})
	if _, err := f.service.CallTool(t.Context(), api.Actor{Identity: "dev@host"}, "promote", raw); !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "tag_after requires an admin uplink") {
		t.Fatalf("non-admin tag_after = %v", err)
	}
	if len(git.pushes) != 0 {
		t.Fatalf("pushes = %v", git.pushes)
	}
}

// A metrics-scoped uplink is served no tool, whatever the caller (#1534).
func TestMetricsScopedActorIsServedNoTool(t *testing.T) {
	f, _ := newTagPromotionFixture(t)
	for _, tool := range []string{"run_list", "status", "repo_list", "uplink_list", "events_since", "promote"} {
		for _, admin := range []bool{false, true} {
			if _, err := f.service.CallTool(t.Context(), api.Actor{Identity: "metrics@prometheus", Admin: admin, Scope: []string{"metrics"}}, tool, json.RawMessage(`{}`)); !errors.Is(err, ErrForbidden) {
				t.Fatalf("%s admin=%v = %v", tool, admin, err)
			}
		}
	}
}

// A promotion that failed while it was admitted is announced before promote
// records the request; the pending sweep still settles it.
func TestPromotionTagAfterRequestedAfterTheEventIsSettled(t *testing.T) {
	f, git := newTagPromotionFixture(t)
	promotion, err := f.store.AppendPromotion(t.Context(), model.PromotionSpec{RepoID: f.repo.ID, SourceBranch: "feature/tag", SourceSHA: f.initial, TargetRef: "main", Actor: "dev@host"})
	if err != nil {
		t.Fatal(err)
	}
	promotion.Status, promotion.Error = model.PromotionFailed, "prepare promotion: conflict"
	recorder := f.service.agentEvents
	recorder.started = time.Now()
	recorder.observePromotion(t.Context(), promotion)
	if !recorder.recorded(model.AgentEventPromotionFinished + ":" + promotion.ID) {
		t.Fatal("promotion.finished not recorded")
	}
	if err := f.store.RequestPromotionTag(t.Context(), promotion.ID, "dev@host", "deploy/1", "apply it"); err != nil {
		t.Fatal(err)
	}
	recorder.observePromotionRequests(t.Context(), promotion, true)
	tag, err := f.store.PromotionTag(t.Context(), promotion.ID)
	if err != nil || tag.Status != model.PromotionTagFailed || !strings.Contains(tag.Reason, "promotion failed") || len(git.pushes) != 0 {
		t.Fatalf("late request = %+v %v", tag, err)
	}
}

func TestPromoteTagAfterRefusesReleaseVersionNames(t *testing.T) {
	f, git := newTagPromotionFixture(t)
	for _, name := range []string{"v1.2.3", "v1.2.3-rc.1", "v1.2.3+build", "v01.2.3", "v1.2.3-extra"} {
		raw, _ := json.Marshal(map[string]any{"repo": f.repo.Name, "branch": "main", "sha": f.initial, "tag_after": map[string]any{"name": name, "message": "release"}})
		if _, err := f.service.CallTool(t.Context(), pruneAdmin, "promote", raw); !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "release-version") {
			t.Fatalf("release name %q: %v", name, err)
		}
	}
	if len(git.pushes) != 0 {
		t.Fatal("refused names were pushed")
	}
}

func (g *taggingGit) UpstreamContainsCommit(context.Context, string, string, string) (bool, error) {
	return g.contains, nil
}

func TestPromotionTagRetryRechecksMovedTargetAndAuditsIntent(t *testing.T) {
	f, g := newTagPromotionFixture(t)
	g.head = f.initial
	g.err = fmt.Errorf("push: %w", gitssh.ErrTransportUnavailable)
	promotion, request := f.taggedPromotion(t, model.PromotionPassed, f.initial)
	request = f.service.settlePromotionTag(t.Context(), f.store, promotion, request, time.Now())
	if request.NextRetryAt == nil {
		t.Fatal("no retry scheduled")
	}
	g.head = f.unmerged
	g.contains = true
	g.err = nil
	g.beforePush = func() {
		actions, _, err := f.store.ListAuditActions(t.Context(), model.AuditListFilter{ActionPrefix: "promotion.tag_after.push.intent", Limit: 20})
		if err != nil || len(actions) != 2 {
			t.Fatalf("intent missing before retry push: %d %v", len(actions), err)
		}
	}
	got := f.service.settlePromotionTag(t.Context(), f.store, promotion, request, request.NextRetryAt.Add(time.Second))
	if got.Status != model.PromotionTagPushed || !strings.Contains(got.Reason, "contains") {
		t.Fatalf("advanced target retry: %+v", got)
	}
	g.beforePush = nil
	g.pushes = nil
	f.service.auditor = nil
	promotion, request = f.taggedPromotion(t, model.PromotionPassed, f.initial)
	got = f.service.settlePromotionTag(t.Context(), f.store, promotion, request, time.Now())
	if got.Status == model.PromotionTagPushed || len(g.pushes) != 0 {
		t.Fatal("push ran without durable intent")
	}
}

func (g *taggingGit) Ensure(context.Context, string) (gitcache.Repository, error) {
	return gitcache.Repository{DefaultBranch: "main"}, nil
}
func (g *taggingGit) PeelObject(_ context.Context, _, object string) (gitcache.PeeledObject, error) {
	return gitcache.PeeledObject{ObjectSHA: object, CommitSHA: g.commit, ObjectType: "tag"}, nil
}
func (g *taggingGit) ReleaseReachable(context.Context, string, string, string) (bool, error) {
	return !g.unreachable, nil
}
func (g *taggingGit) RemoteRef(ctx context.Context, repo, ref string) (string, bool, error) {
	if strings.HasPrefix(ref, "refs/tags/") {
		return strings.Repeat("e", 40), g.published, nil
	}
	return g.upstreamGit.RemoteRef(ctx, repo, ref)
}
