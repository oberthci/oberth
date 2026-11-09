package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// upstreamGit answers RemoteRef with a fixed upstream head.
type upstreamGit struct {
	DeliveryGit
	head string
	err  error
}

func (g *upstreamGit) RemoteRef(context.Context, string, string) (string, bool, error) {
	return g.head, g.head != "", g.err
}

func newPrunePromotionFixture(t *testing.T) (*pruneFixture, *upstreamGit) {
	t.Helper()
	f := newPruneFixture(t)
	git := &upstreamGit{DeliveryGit: f.controlFixture.git}
	service, err := NewAPI(APIConfig{
		Runs: f.store, History: f.store, Repositories: f.store, Issues: f.store, Promotions: f.store, PromotionRuns: f.store,
		Enqueues: f.scheduler, Git: git, Logs: f.logs, Auditor: f.store,
		BranchPrune: f.cache, BranchDeletions: f.store, DeletionQueued: func() { f.queued++ },
		AgentEvents: f.store,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.service = service
	return f, git
}

func (f *pruneFixture) promotion(t *testing.T, branch, sha string, status model.PromotionStatus, result string) (model.Promotion, model.PromotionPrune) {
	t.Helper()
	promotion, err := f.store.AppendPromotion(t.Context(), model.PromotionSpec{RepoID: f.repo.ID, SourceBranch: branch, SourceSHA: sha, TargetRef: "main", Actor: "dev@host"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.RequestPromotionPrune(t.Context(), promotion.ID, "dev@host"); err != nil {
		t.Fatal(err)
	}
	request, err := f.store.PromotionPrune(t.Context(), promotion.ID)
	if err != nil {
		t.Fatal(err)
	}
	promotion.Status, promotion.ResultSHA = status, result
	return promotion, request
}

func (f *pruneFixture) mirrorHas(t *testing.T, branch string) bool {
	return f.git(t, f.mirror, "for-each-ref", "refs/heads/"+branch) != ""
}

// prune_source prunes only after a passed promotion whose result is the
// upstream target, and keeps the branch with a recorded reason otherwise.
func TestPromotionPruneSourceOnlyAfterVerifiedPromotion(t *testing.T) {
	f, git := newPrunePromotionFixture(t)
	now := time.Now()
	settle := func(promotion model.Promotion, request model.PromotionPrune) model.PromotionPrune {
		return f.service.settlePromotionPrune(t.Context(), f.store, promotion, request, now)
	}

	failed, request := f.promotion(t, "feature/a", f.initial, model.PromotionFailed, "")
	if got := settle(failed, request); got.Status != model.PromotionPruneKept || !strings.Contains(got.Reason, "promotion failed") || !f.mirrorHas(t, "feature/a") {
		t.Fatalf("failed promotion = %+v", got)
	}

	git.head = f.unmerged
	moved, request := f.promotion(t, "feature/b", f.initial, model.PromotionPassed, f.initial)
	if got := settle(moved, request); got.Status != model.PromotionPruneKept || !strings.Contains(got.Reason, "not the promotion result") || !f.mirrorHas(t, "feature/b") {
		t.Fatalf("target differs = %+v", got)
	}

	git.head, git.err = "", errors.New("connection reset")
	unreachable, request := f.promotion(t, "feature/c", f.initial, model.PromotionPassed, f.initial)
	if got := settle(unreachable, request); got.Status != model.PromotionPrunePending || !f.mirrorHas(t, "feature/c") {
		t.Fatalf("unreachable upstream must stay pending = %+v", got)
	}
	if got := f.service.settlePromotionPrune(t.Context(), f.store, unreachable, request, now.Add(2*agentEventPruneGiveUp)); got.Status != model.PromotionPruneKept {
		t.Fatalf("give up = %+v", got)
	}

	git.head, git.err = f.unmerged, nil
	unmerged, request := f.promotion(t, "feature/unmerged", f.unmerged, model.PromotionPassed, f.unmerged)
	if got := settle(unmerged, request); got.Status != model.PromotionPruneKept || !strings.Contains(got.Reason, "not merged") || !f.mirrorHas(t, "feature/unmerged") {
		t.Fatalf("unmerged source = %+v", got)
	}

	git.head = f.initial
	passed, request := f.promotion(t, "feature/a", f.initial, model.PromotionPassed, f.initial)
	got := settle(passed, request)
	if got.Status != model.PromotionPrunePruned || got.BatchID == "" || f.mirrorHas(t, "feature/a") || f.queued != 1 {
		t.Fatalf("verified promotion = %+v queued=%d", got, f.queued)
	}
	// A settled request never changes again.
	if again := settle(passed, got); again.Status != model.PromotionPrunePruned || again.BatchID != got.BatchID {
		t.Fatalf("settled request changed: %+v", again)
	}
	// promotion.finished carries the prune outcome.
	f.service.agentEvents.started = now
	f.service.agentEvents.observePromotion(t.Context(), passed)
	events, err := f.store.AgentEvents(t.Context(), model.AgentEventFilter{Kinds: []string{model.AgentEventPromotionFinished}})
	if err != nil || len(events) != 1 {
		t.Fatalf("promotion events = %+v, %v", events, err)
	}
	if prune, _ := events[0].Data["prune_source"].(map[string]any); prune["status"] != model.PromotionPrunePruned || !strings.Contains(events[0].Summary, "source pruned") {
		t.Fatalf("promotion event = %+v", events[0])
	}
	actions, _, err := f.store.ListAuditActions(t.Context(), model.AuditListFilter{ActionPrefix: "promotion.prune_source.", Limit: 20})
	if err != nil || len(actions) != 5 {
		t.Fatalf("audit = %d %v", len(actions), err)
	}
}

// promote refuses prune_source for a train or a non-default target before
// anything is admitted.
func TestPromotePruneSourceRefusals(t *testing.T) {
	f, _ := newPrunePromotionFixture(t)
	call := func(arguments map[string]any) error {
		raw, _ := json.Marshal(arguments)
		_, err := f.service.CallTool(t.Context(), pruneAdmin, "promote", raw)
		return err
	}
	if err := call(map[string]any{"repo": f.repo.Name, "branch": "main", "sources": []map[string]string{{"sha": f.initial}}, "prune_source": true}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("train = %v", err)
	}
	if err := call(map[string]any{"repo": f.repo.Name, "branch": "release", "sha": f.initial, "prune_source": true}); err == nil {
		t.Fatal("non-default target accepted")
	}
}

// Confirming a deletion of unmerged branches needs a reason, which the
// batch audit records.
func TestBranchDeleteManyUnmergedNeedsReason(t *testing.T) {
	f := newPruneFixture(t)
	selection := map[string]any{"repo": f.repo.Name, "branches": []map[string]string{{"branch": "feature/unmerged", "expected_sha": f.unmerged}}, "allow_unmerged": true}
	plan, err := f.prune(t, pruneAdmin, selection)
	if err != nil || len(plan.Unmerged) != 1 {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	selection["confirm"], selection["plan_id"] = true, plan.PlanID
	if _, err := f.prune(t, pruneAdmin, selection); !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "reason") {
		t.Fatalf("unmerged without reason = %v", err)
	}
	if !f.mirrorHas(t, "feature/unmerged") {
		t.Fatal("refused confirmation deleted the branch")
	}
	selection["reason"] = "abandoned experiment, recovered by sha if needed"
	if executed, err := f.prune(t, pruneAdmin, selection); err != nil || executed.Deleted != 1 {
		t.Fatalf("with reason = %+v, %v", executed, err)
	}
	actions, _, err := f.store.ListAuditActions(t.Context(), model.AuditListFilter{ActionPrefix: "branch.prune", Limit: 5})
	if err != nil || len(actions) != 1 || !strings.Contains(actions[0].Details, "abandoned experiment") || !strings.Contains(actions[0].Details, `"unmerged":1`) {
		t.Fatalf("audit = %+v, %v", actions, err)
	}
}
