package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/storetest"
)

func TestOrdinaryTagAdmissionSharesReceiveQueueWithoutReleaseAuthority(t *testing.T) {
	object, commit := strings.Repeat("e", 40), strings.Repeat("a", 40)
	for _, published := range []bool{false, true} {
		t.Run(map[bool]string{false: "receive before publication", true: "tag already upstream"}[published], func(t *testing.T) {
			queue, releases := &stubCIQueue{}, &stubReleaseQueue{}
			git := &stubPushGit{peeled: gitcache.PeeledObject{ObjectSHA: object, CommitSHA: commit, ObjectType: "tag"}, reachable: true, remoteSHA: object, remoteExists: published}
			ingestor, err := NewPushIngestor(PushConfig{Repositories: stubPushRepositories{repository: model.Repository{ID: 1, Name: "repo", DefaultBranch: "main"}}, Discoverer: stubRepositoryDiscoverer{}, Git: git, Receives: &stubReceiveRecorder{}, Branches: queue, Releases: releases})
			if err != nil {
				t.Fatal(err)
			}
			adapter, _ := NewReceiveHandler(ingestor)
			err = adapter.HandleReceive(t.Context(), gitcache.ReceiveEvent{ID: "ordinary", Repo: "repo", Actor: "developer", Update: gitcache.RefUpdate{Ref: "refs/tags/deploy-1", NewSHA: object}, ReleaseAdmission: gitcache.ReleaseAdmission{SHA: commit}})
			if err != nil {
				t.Fatal(err)
			}
			if len(queue.requests) != 1 || len(releases.requests) != 0 {
				t.Fatal("ordinary tag acquired release admission")
			}
			request := queue.requests[0]
			if request.RefKind != model.RefTag || request.Trigger != "tag" || request.SHA != object || request.TestedSHA != commit || request.Branch != "deploy-1" {
				t.Fatalf("lost tag provenance: %+v", request)
			}
		})
	}
}

type failPromotionTagSettlement struct {
	AgentEventStore
	fail bool
}

func (s *failPromotionTagSettlement) FinishPromotionTag(ctx context.Context, id, status, reason, object string, attempts int) (model.PromotionTag, error) {
	if s.fail {
		s.fail = false
		return model.PromotionTag{}, errors.New("settlement unavailable")
	}
	return s.AgentEventStore.FinishPromotionTag(ctx, id, status, reason, object, attempts)
}

func TestPromotionTagQueuesOnceAcrossCrashAfterAdmission(t *testing.T) {
	f, g := newTagPromotionFixture(t)
	g.head = f.initial
	promotion, request := f.taggedPromotion(t, model.PromotionPassed, f.initial)
	tags := &failPromotionTagSettlement{AgentEventStore: f.store, fail: true}
	first := f.service.settlePromotionTag(t.Context(), tags, promotion, request, time.Now())
	if first.Status != model.PromotionTagPending {
		t.Fatal("failed settlement should leave durable request pending")
	}
	receipt, err := f.store.ReceiveEvent(t.Context(), "promotion-tag:"+promotion.ID)
	if err != nil || receipt.RunID == "" {
		t.Fatal("tag run was not committed before settlement")
	}
	run, err := f.store.Run(t.Context(), receipt.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Release || run.RefKind != model.RefTag || run.Trigger != "tag" || run.SHA != strings.Repeat("e", 40) || run.TestedSHA != f.initial {
		t.Fatalf("wrong tag run: %+v", run)
	}
	// Replay the pending request after an upstream rewind. The already committed
	// receive decision wins; neither a new push nor another run is allowed.
	g.head = f.unmerged
	g.contains = false
	got := f.service.settlePromotionTag(t.Context(), tags, promotion, first, time.Now())
	if got.Status != model.PromotionTagPushed || !strings.Contains(got.Reason, receipt.RunID) || len(g.pushes) != 1 {
		t.Fatalf("settlement replay: %+v, pushes %v", got, g.pushes)
	}
	runs, err := f.store.ListRecentRuns(t.Context(), model.RunListFilter{RepoID: f.repo.ID, Ref: "deploy/1"})
	if err != nil || len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatal("duplicate tag CI after replay")
	}
}

func TestPromotionTagRefusesReleaseRoutesBeforeChangingRefs(t *testing.T) {
	f, g := newTagPromotionFixture(t)
	g.head = f.initial
	f.service.tags.releaseTagRoute = func(string, string) (bool, error) { return true, nil }
	seed, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "feature/test", SHA: f.initial, Actor: "admin", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.FinishRun(t.Context(), seed.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"repo":"` + f.repo.Name + `","branch":"main","sha":"` + f.initial + `","tag_after":{"name":"deploy-1","message":"deploy"}}`)
	if _, err := f.service.CallTool(t.Context(), api.Actor{Identity: "admin", Admin: true}, "promote", raw); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("administrator release route was accepted for tag_after")
	}
	promotion, request := f.taggedPromotion(t, model.PromotionPassed, f.initial)
	got := f.service.settlePromotionTag(t.Context(), f.store, promotion, request, time.Now())
	if got.Status != model.PromotionTagFailed || len(g.pushes) != 0 {
		t.Fatal("release-routed tag was pushed")
	}
}

func TestPromotionTagMustReachFreshDefaultBranchBeforePush(t *testing.T) {
	f, g := newTagPromotionFixture(t)
	g.head = f.initial
	g.unreachable = true
	promotion, request := f.taggedPromotion(t, model.PromotionPassed, f.initial)
	got := f.service.settlePromotionTag(t.Context(), f.store, promotion, request, time.Now())
	if got.Status != model.PromotionTagFailed || len(g.pushes) != 0 {
		t.Fatal("tag outside fresh default branch was pushed")
	}
}

func TestPromotionTagRealPublisherEnqueuesOrdinaryTagRun(t *testing.T) {
	f := newPruneFixture(t)
	pushes, err := NewPushIngestor(PushConfig{Repositories: f.store, Discoverer: stubRepositoryDiscoverer{}, Git: f.cache, Receives: f.store, Branches: f.scheduler, Releases: f.scheduler, Auditor: f.store})
	if err != nil {
		t.Fatal(err)
	}
	control, err := NewAPI(APIConfig{Tags: pushes, Runs: f.store, History: f.store, Repositories: f.store, Git: f.cache, Auditor: f.store, AgentEvents: f.store})
	if err != nil {
		t.Fatal(err)
	}
	promotion, request := f.taggedPromotion(t, model.PromotionPassed, f.initial)
	got := control.settlePromotionTag(t.Context(), f.store, promotion, request, time.Now())
	if got.Status != model.PromotionTagPushed {
		t.Fatalf("real tag push: %+v", got)
	}
	object := f.git(t, f.upstream, "rev-parse", "refs/tags/deploy/1")
	event, err := f.store.ReceiveEvent(t.Context(), "promotion-tag:"+promotion.ID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.store.Run(t.Context(), event.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.SHA != object || run.TestedSHA != f.initial || run.Release || run.RefKind != model.RefTag {
		t.Fatalf("real publisher admission: %+v", run)
	}
}

type failingTagCIQueue struct {
	CIQueue
	fail bool
}

func (q *failingTagCIQueue) EnqueueCI(ctx context.Context, r CIRequest) (model.EnqueueRunResult, error) {
	if q.fail {
		q.fail = false
		return model.EnqueueRunResult{}, ErrUnavailable
	}
	return q.CIQueue.EnqueueCI(ctx, r)
}
func TestPromotionTagRetriesAdmissionAfterSuccessfulPush(t *testing.T) {
	f, g := newTagPromotionFixture(t)
	g.head = f.initial
	f.service.tags.branches = &failingTagCIQueue{CIQueue: f.scheduler, fail: true}
	promotion, request := f.taggedPromotion(t, model.PromotionPassed, f.initial)
	first := f.service.settlePromotionTag(t.Context(), f.store, promotion, request, time.Now())
	if first.Status != model.PromotionTagPending || first.NextRetryAt == nil || !g.published {
		t.Fatal("successful push lost the pending admission retry")
	}
	got := f.service.settlePromotionTag(t.Context(), f.store, promotion, first, first.NextRetryAt.Add(time.Second))
	if got.Status != model.PromotionTagPushed {
		t.Fatalf("admission retry: %+v", got)
	}
	runs, err := f.store.ListRecentRuns(t.Context(), model.RunListFilter{RepoID: f.repo.ID, Ref: "deploy/1"})
	if err != nil || len(runs) != 1 || runs[0].Release {
		t.Fatal("admission retry did not create one ordinary tag run")
	}
}

func TestOrdinaryTagRerunKeepsBuildTier(t *testing.T) {
	run := model.Run{ID: "tag-ci", RefKind: model.RefTag, Trigger: "tag"}
	warning, err := RunRerunPolicy(run, true, true, false)
	if err != nil || !strings.Contains(warning, "build.yaml") || strings.Contains(warning, "release.yaml") {
		t.Fatal("ordinary tag rerun described release execution")
	}
	if _, err := RunRerunPolicy(run, false, true, true); !errors.Is(err, ErrForbidden) {
		t.Fatal("ordinary tag rerun bypassed admin policy")
	}
}

func TestTagReleaseRouteUsesCanonicalRepositoryForShortPushAlias(t *testing.T) {
	f := newControlFixture(t)
	canonical, err := f.store.QualifiedRepoName(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	object, commit := strings.Repeat("e", 40), strings.Repeat("a", 40)
	queue, releases := &stubCIQueue{}, &stubReleaseQueue{}
	git := &stubPushGit{peeled: gitcache.PeeledObject{ObjectSHA: object, CommitSHA: commit, ObjectType: "tag"}, reachable: true}
	var classified string
	ingestor, err := NewPushIngestor(PushConfig{Repositories: f.store, Discoverer: stubRepositoryDiscoverer{}, Git: git, Receives: f.store, Branches: queue, Releases: releases, ReleaseTagRoute: func(repo, tag string) (bool, error) {
		classified = repo
		return repo == canonical && tag == "deploy-1", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingestor.Tag(t.Context(), TagPush{EventID: "short-alias-route", Repository: f.repo.Name, Tag: "deploy-1", NewOID: object, Actor: "admin", ReleaseAdmissionSHA: commit}); err != nil {
		t.Fatal(err)
	}
	if classified != canonical || len(queue.requests) != 0 || len(releases.requests) != 1 {
		t.Fatal("short repository spelling bypassed administrator release policy")
	}
}
