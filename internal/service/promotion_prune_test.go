package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

	git.head, git.err = f.initial, nil
	unmerged, request := f.promotion(t, "feature/unmerged", f.unmerged, model.PromotionPassed, f.initial)
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

// A prune request left pending across a restart (the upstream was
// unreachable, or the server crashed) is still settled by the recorder even
// when the promotion finished before the recorder's event lookback.
func TestPromotionPruneSettlesPendingRequestBeyondLookback(t *testing.T) {
	f, git := newPrunePromotionFixture(t)
	git.head = f.initial
	promotion, _ := f.promotion(t, "feature/a", f.initial, model.PromotionPassed, f.initial)
	now := time.Now()
	promotion.UpdatedAt = now.Add(-2 * agentEventLookback)
	f.service.agentEvents.started = now
	f.service.agentEvents.observePromotion(t.Context(), promotion)
	request, err := f.store.PromotionPrune(t.Context(), promotion.ID)
	if err != nil || request.Status != model.PromotionPrunePruned || f.mirrorHas(t, "feature/a") {
		t.Fatalf("pending prune beyond the lookback = %+v, %v", request, err)
	}
	events, err := f.store.AgentEvents(t.Context(), model.AgentEventFilter{Kinds: []string{model.AgentEventPromotionFinished}})
	if err != nil || len(events) != 1 {
		t.Fatalf("a settled prune emits promotion.finished once: %+v, %v", events, err)
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

// prune_source never deletes the default branch, the target or a reserved
// (protected) branch, even after a verified promotion (#1296).
func TestPromotionPruneSourceNeverPrunesDefaultOrReservedBranches(t *testing.T) {
	f, git := newPrunePromotionFixture(t)
	git.head = f.initial
	for _, branch := range []string{"main", "master"} {
		promotion, request := f.promotion(t, branch, f.initial, model.PromotionPassed, f.initial)
		got := f.service.settlePromotionPrune(t.Context(), f.store, promotion, request, time.Now())
		if got.Status != model.PromotionPruneKept || !strings.Contains(got.Reason, "never pruned") || !f.mirrorHas(t, branch) {
			t.Fatalf("%s = %+v", branch, got)
		}
	}
	if f.queued != 0 {
		t.Fatalf("deletion queued %d times", f.queued)
	}
}

// A real publication passes before pruning, while the mirror's forge tracking
// ref deliberately still points at the pre-promotion target (#1562).
func TestPromotionPruneUsesPublishedTarget(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		merge, fresh, moved, missing bool
	}{
		{name: "fast-forward stale target"},
		{name: "fast-forward refreshed target", fresh: true},
		{name: "merge commit stale target", merge: true},
		{name: "source advanced after promotion", moved: true},
		{name: "missing cached target", missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newPrunePromotionFixture(t)
			f.git(t, f.work, "checkout", "feature/unmerged")
			if err := os.WriteFile(filepath.Join(f.work, "promoted.txt"), []byte("promoted work\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			f.git(t, f.work, "add", "promoted.txt")
			f.git(t, f.work, "commit", "-m", "source work")
			source := f.git(t, f.work, "rev-parse", "HEAD")
			result := source
			if tc.merge {
				f.git(t, f.work, "checkout", "main")
				f.git(t, f.work, "merge", "--no-ff", "--no-edit", source)
				result = f.git(t, f.work, "rev-parse", "HEAD")
			}
			// Seed immutable objects without refreshing the forge tracking ref or
			// moving mirror main: even the mirror target may still be stale.
			f.git(t, f.mirror, "fetch", "--no-tags", f.work, result)
			f.git(t, f.mirror, "update-ref", "refs/heads/feature/unmerged", source)
			cachedTarget := "refs/oberth/upstream/heads/main"
			if tc.fresh {
				f.git(t, f.mirror, "update-ref", cachedTarget, result)
			}
			if tc.missing {
				f.git(t, f.mirror, "update-ref", "-d", cachedTarget)
			}

			promotion, err := f.store.AppendPromotion(t.Context(), model.PromotionSpec{
				RepoID: f.repo.ID, SourceBranch: "feature/unmerged", SourceSHA: source,
				TargetRef: "main", PreviousSHA: f.initial, ResultSHA: result, Actor: "dev@host",
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.store.RequestPromotionPrune(t.Context(), promotion.ID, promotion.Actor); err != nil {
				t.Fatal(err)
			}
			publication, err := f.store.BeginPublication(t.Context(), model.PublicationSpec{
				RepoID: f.repo.ID, PromotionID: promotion.ID, RefKind: model.RefBranch,
				Ref: "main", PreviousSHA: f.initial, ResultSHA: result, Actor: promotion.Actor,
			})
			if err != nil {
				t.Fatal(err)
			}
			f.controlFixture.git.setRemoteRefLocked("refs/heads/main", f.initial)
			forge := &confirmationGit{controlGit: f.controlFixture.git, after: result}
			f.service.git = forge
			if _, err := deliverPublication(t.Context(), f.store, forge, f.store, f.repo, f.input, publication); err != nil {
				t.Fatal(err)
			}
			promotion, err = f.store.Promotion(t.Context(), promotion.ID)
			if err != nil || promotion.Status != model.PromotionPassed || promotion.ObservedTargetSHA != result || forge.pushed != 1 {
				t.Fatalf("publication did not pass: %+v, %v", promotion, err)
			}
			if tc.moved {
				f.git(t, f.work, "checkout", "feature/unmerged")
				f.git(t, f.work, "commit", "--allow-empty", "-m", "later work")
				head := f.git(t, f.work, "rev-parse", "HEAD")
				f.git(t, f.mirror, "fetch", "--no-tags", f.work, head)
				f.git(t, f.mirror, "update-ref", "refs/heads/feature/unmerged", head)
			}
			request, err := f.store.PromotionPrune(t.Context(), promotion.ID)
			if err != nil {
				t.Fatal(err)
			}
			got := f.service.settlePromotionPrune(t.Context(), f.store, promotion, request, time.Now())
			if tc.moved {
				if got.Status != model.PromotionPruneKept || !strings.Contains(got.Reason, "head moved") || !f.mirrorHas(t, "feature/unmerged") || f.queued != 0 {
					t.Fatalf("moved source = %+v, queued=%d", got, f.queued)
				}
			} else if got.Status != model.PromotionPrunePruned || got.BatchID == "" || f.mirrorHas(t, "feature/unmerged") || f.queued != 1 {
				t.Fatalf("published source = %+v, queued=%d", got, f.queued)
			}
			// Pruning must not manufacture a new cached observation of the forge.
			if !tc.fresh && !tc.missing && f.git(t, f.mirror, "rev-parse", cachedTarget) != f.initial {
				t.Fatal("pruning changed the cached target ref")
			}
		})
	}
}
