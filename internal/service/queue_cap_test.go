package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

func TestQueueCapAdminToolIsStrictAndGated(t *testing.T) {
	f := newControlFixture(t)
	control := f.api(t)
	ctx := t.Context()
	admin := api.Actor{Identity: "admin", Admin: true}
	for _, actor := range []api.Actor{{Identity: "dev"}, {Identity: "scoped", Admin: true, Scope: []string{"codeberg"}}} {
		if _, err := control.CallTool(ctx, actor, "queue_cap_config", json.RawMessage(`{"max_not_started":1}`)); !errors.Is(err, ErrForbidden) {
			t.Fatalf("unauthorized cap config: %v", err)
		}
	}
	if _, err := control.CallTool(ctx, admin, "queue_cap_config", json.RawMessage(`{"max_not_started":1}`)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{}`, `{"max_not_started":-1}`, `{"max_not_started":1.5}`, `{"max_not_started":null}`, `{"max_not_started":1,"extra":true}`} {
		if _, err := control.CallTool(ctx, admin, "queue_cap_config", json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	denied := errors.New("audit gate refused")
	control.mutationGate = func(_ context.Context) error { return denied }
	if _, err := control.CallTool(ctx, admin, "queue_cap_config", json.RawMessage(`{"max_not_started":0}`)); !errors.Is(err, denied) {
		t.Fatalf("cap bypassed gate: %v", err)
	}
	if !MutatingTool("queue_cap_config") {
		t.Fatal("cap configuration must be a mutation")
	}
}

func TestQueueCapDeferredPushStaysLocalAndVisibleWithoutRun(t *testing.T) {
	f := newControlFixture(t)
	ctx := t.Context()
	if _, err := f.store.SetQueueCap(ctx, "admin", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.scheduler.EnqueueCI(ctx, CIRequest{EventID: "occupant", Repository: f.repo, Branch: "feature/occupant", SHA: strings.Repeat("b", 40), Actor: "dev"}); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	g := &parkPushGit{stubPushGit: &stubPushGit{peeled: gitcache.PeeledObject{ObjectSHA: sha, CommitSHA: sha}}}
	ingestor, err := NewPushIngestor(PushConfig{Repositories: f.store, Discoverer: stubRepositoryDiscoverer{}, Git: g, Receives: f.store, Branches: f.scheduler, Auditor: f.store})
	if err != nil {
		t.Fatal(err)
	}
	push := BranchPush{EventID: "deferred", Repository: f.repo.Name, Branch: "feature/deferred", NewOID: sha, Actor: "dev"}
	result, err := ingestor.Branch(ctx, push)
	if err != nil || result.Run != nil || result.Reason != "branch run deferred: queue at 1/1" || len(g.mirrored) != 0 {
		t.Fatalf("deferred push: %+v mirrors=%v err=%v", result, g.mirrored, err)
	}
	result, err = ingestor.Branch(ctx, push)
	if err != nil || !result.Duplicate || result.Run != nil {
		t.Fatalf("deferred replay: %+v %v", result, err)
	}
	if _, err := f.store.ResolveRun(ctx, f.repo.ID, sha); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deferred push created run: %v", err)
	}
	// Even a run at the same SHA on another ref must not hide this deferral.
	if _, err := storetest.EnqueueRun(f.store, ctx, model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "feature/other", SHA: sha, Actor: "dev", Trigger: "branch"}); err != nil {
		t.Fatal(err)
	}
	f.refs = stubRefResolver{branches: map[string]map[string]string{"codeberg/acme/oberth": {"feature/deferred": sha}}}
	control := f.api(t)
	value, err := statusHeadCall(t, control, f.repo.Name, push.Branch)
	if err != nil || value.Status != "queued" || value.DisplayStatus != model.DisplayQueued || value.RunID != "" || value.DisplayReason != result.Reason {
		t.Fatalf("deferred status: %+v %v", value, err)
	}
	args, err := json.Marshal(map[string]string{"repo": f.repo.Name, "ref": push.Branch})
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"codeberg", "other"} {
		shown, err := control.CallTool(ctx, api.Actor{Identity: "scoped", Scope: []string{scope}}, "status", args)
		if scope == "other" {
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("out-of-scope deferred status: %+v %v", shown, err)
			}
		} else if err != nil || shown.(StatusResponse).DisplayReason != "branch run deferred: waiting for queue capacity" {
			t.Fatalf("scoped deferred status: %+v %v", shown, err)
		}
	}
	if _, err := f.store.SetQueueCap(ctx, "admin", 0); err != nil {
		t.Fatal(err)
	}
	if got, err := f.store.DrainDeferredBranch(ctx); err != nil || got.ID == "" {
		t.Fatalf("drain: %+v %v", got, err)
	}
	shown, err := control.Runs(ctx, api.Actor{Identity: "scoped", Scope: []string{"codeberg"}}, api.RunFilter{Ref: push.Branch})
	if err != nil {
		t.Fatal(err)
	}
	runs := shown.([]model.Run)
	if len(runs) != 1 || !strings.Contains(runs[0].Reason, "branch run deferred:") || strings.Contains(runs[0].DisplayReason, "1/1") || strings.Contains(runs[0].Reason, "1/1") {
		t.Fatalf("scoped drain reason: %+v", runs)
	}
	result, err = ingestor.Branch(ctx, push)
	if err != nil || !result.Duplicate || result.Run == nil || result.Run.ID != runs[0].ID || len(g.mirrored) != 0 {
		t.Fatalf("drained replay: %+v mirrors=%v err=%v", result, g.mirrored, err)
	}
}

func TestQueueCapDeferralNeverPublishesUpstream(t *testing.T) {
	for _, cell := range []struct {
		name, branch, policy string
		policyErr            error
		withoutMirror        bool
	}{
		{name: "default branch", branch: "main"},
		{name: "required branch CI", branch: "park/review", policy: "require_branch_ci: [park/*]\n"},
		{name: "ordinary branch", branch: "feature/deferred"},
		{name: "unavailable policy", branch: "feature/deferred", policyErr: errors.New("policy unavailable")},
		{name: "adapter without mirror", branch: "feature/deferred", withoutMirror: true},
	} {
		t.Run(cell.name, func(t *testing.T) {
			f := newControlFixture(t)
			ctx := t.Context()
			if _, err := f.store.SetQueueCap(ctx, "admin", 1); err != nil {
				t.Fatal(err)
			}
			if _, err := f.scheduler.EnqueueCI(ctx, CIRequest{EventID: "occupant", Repository: f.repo, Branch: "feature/occupant", SHA: strings.Repeat("b", 40), Actor: "dev"}); err != nil {
				t.Fatal(err)
			}
			sha := strings.Repeat("a", 40)
			g := &parkPushGit{stubPushGit: &stubPushGit{peeled: gitcache.PeeledObject{ObjectSHA: sha, CommitSHA: sha}}, policy: cell.policy, policyErr: cell.policyErr}
			var acceptedGit PushGit = g
			if cell.withoutMirror {
				acceptedGit = g.stubPushGit
			}
			ingestor, err := NewPushIngestor(PushConfig{Repositories: f.store, Discoverer: stubRepositoryDiscoverer{}, Git: acceptedGit, Receives: f.store, Branches: f.scheduler, Auditor: f.store})
			if err != nil {
				t.Fatal(err)
			}
			push := BranchPush{EventID: "deferred", Repository: f.repo.Name, Branch: cell.branch, NewOID: sha, Actor: "dev"}
			for attempt := range 2 {
				result, err := ingestor.Branch(ctx, push)
				if err != nil || result.Run != nil || result.Reason != "branch run deferred: queue at 1/1" || result.Duplicate != (attempt > 0) || len(g.mirrored) != 0 {
					t.Fatalf("attempt %d: result=%+v mirrors=%v err=%v", attempt, result, g.mirrored, err)
				}
			}
			cap, err := f.store.QueueCap(ctx)
			if err != nil || cap.Deferred != 1 || cap.NotStarted != 1 {
				t.Fatalf("deferral was not recorded exactly once: %+v %v", cap, err)
			}
			if _, err := f.store.LatestAuditActionByType(ctx, "branch.defer.mirror", "repository"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("deferral recorded an upstream mirror audit: %v", err)
			}
		})
	}
}

func TestQueueCapSchedulerAutomaticallyDrainsOnStartupInArrivalOrder(t *testing.T) {
	green := JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)}}
	f := newControlFixture(t, green, green, green)
	ctx := t.Context()
	if _, err := f.store.SetQueueCap(ctx, "admin", 1); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"occupant", "z-first", "a-second"} {
		got, err := f.scheduler.EnqueueCI(ctx, CIRequest{EventID: name, Repository: f.repo, Branch: "feature/" + name, SHA: strings.Repeat("a", 40), Actor: "dev"})
		if err != nil || (got.ID == "") != (name != "occupant") {
			t.Fatalf("enqueue %s: %+v %v", name, got, err)
		}
	}
	ownerCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- f.scheduler.Run(ownerCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(testStartTimeout(5 * time.Second)):
			t.Error("scheduler did not stop")
		}
	})
	deadline := time.NewTimer(testStartTimeout(10 * time.Second))
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		runs, err := f.store.ListRecentRuns(ctx, model.RunListFilter{RepoID: f.repo.ID, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		allPassed := len(runs) == 3
		for _, run := range runs {
			allPassed = allPassed && run.Status == model.RunPassed
		}
		if allPassed {
			var sequences = map[string]int64{}
			for _, run := range runs {
				sequences[run.Ref] = run.QueueSequence
			}
			if sequences["feature/z-first"] >= sequences["feature/a-second"] {
				t.Fatalf("drain reordered arrivals: %+v", runs)
			}
			cap, err := f.store.QueueCap(ctx)
			if err != nil || cap.Deferred != 0 || cap.NotStarted != 0 {
				t.Fatalf("drained cap: %+v %v", cap, err)
			}
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("automatic drain did not finish: %+v", runs)
		case <-tick.C:
		}
	}
}

func TestQueueCapTagAndPromotionAdmissionBypassFullQueue(t *testing.T) {
	f := newControlFixture(t)
	ctx := t.Context()
	if _, err := f.store.SetQueueCap(ctx, "admin", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.scheduler.EnqueueCI(ctx, CIRequest{EventID: "occupant", Repository: f.repo, Branch: "feature/occupant", SHA: strings.Repeat("b", 40), Actor: "dev"}); err != nil {
		t.Fatal(err)
	}
	tag, err := f.scheduler.AdmitRelease(ctx, ReleaseRequest{EventID: "release", Repository: f.repo, Tag: "v1.0.0", ObjectSHA: strings.Repeat("c", 40), CommitSHA: strings.Repeat("d", 40), Actor: "dev"})
	if err != nil || tag.ID == "" || !tag.Release {
		t.Fatalf("full queue deferred release: %+v %v", tag, err)
	}
	f.git.plan = gitcache.MergeCandidate{BaseSHA: strings.Repeat("b", 40), MergedSHA: strings.Repeat("a", 40), FastForward: true}
	control := f.api(t)
	control.git = &parkDeliveryGit{DeliveryGit: f.git, ref: "park/review"}
	promotion, err := control.promote(ctx, api.Actor{Identity: "admin", Admin: true}, f.repo.Name, strings.Repeat("a", 40), "main")
	if err != nil || promotion.RunID == "" || promotion.Status != model.PromotionPending {
		t.Fatalf("full queue deferred promotion: %+v %v", promotion, err)
	}
	cap, err := f.store.QueueCap(ctx)
	if err != nil || cap.NotStarted != 3 || cap.Deferred != 0 {
		t.Fatalf("bypassed count: %+v %v", cap, err)
	}
}
