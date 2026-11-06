package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func TestParkFallbackReasonSurvivesReceiptAndQueueCap(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		for _, invalid := range []bool{false, true} {
			t.Run(fmt.Sprintf("deferred=%v/invalid=%v", deferred, invalid), func(t *testing.T) {
				f := newControlFixture(t)
				ctx := t.Context()
				if deferred {
					if _, err := f.store.SetQueueCap(ctx, "admin", 1); err != nil {
						t.Fatal(err)
					}
					if _, err := f.scheduler.EnqueueCI(ctx, CIRequest{EventID: "occupant", Repository: f.repo, Branch: "feature/occupant", SHA: strings.Repeat("b", 40), Actor: "dev"}); err != nil {
						t.Fatal(err)
					}
				}
				sha := strings.Repeat("a", 40)
				g := &parkPushGit{stubPushGit: &stubPushGit{peeled: gitcache.PeeledObject{ObjectSHA: sha, CommitSHA: sha}}, policyErr: context.DeadlineExceeded}
				cause := "deadline exceeded"
				if invalid {
					g.policyErr, g.policy = nil, "allow_without_branch_ci: invalid\n"
					cause = "invalid promotion policy"
				}
				p, err := NewPushIngestor(PushConfig{Repositories: f.store, Discoverer: stubRepositoryDiscoverer{}, Git: g, Receives: f.store, Branches: f.scheduler, Auditor: f.store})
				if err != nil {
					t.Fatal(err)
				}
				push := BranchPush{EventID: "fallback", Repository: f.repo.Name, Branch: "park/review", NewOID: sha, Actor: "dev"}
				result, err := p.Branch(ctx, push)
				if err != nil || len(g.mirrored) != 0 {
					t.Fatalf("fallback result=%+v mirrored=%v err=%v", result, g.mirrored, err)
				}
				actions, _, err := f.store.ListAuditActions(ctx, model.AuditListFilter{ActionPrefix: "push.branch.queued"})
				if err != nil {
					t.Fatal(err)
				}
				var reason string
				for _, action := range actions {
					var fields map[string]any
					if err := json.Unmarshal([]byte(action.Details), &fields); err != nil {
						t.Fatal(err)
					}
					if fields["event_id"] == push.EventID {
						reason, _ = fields["reason"].(string)
					}
				}
				if !strings.Contains(reason, "branch CI: park policy unavailable") || !strings.Contains(reason, cause) {
					t.Errorf("receive audit lost fallback cause: %q", reason)
				}
				if deferred {
					if result.Run != nil {
						t.Fatalf("queue cap bypassed: %+v", result)
					}
					if _, err := f.store.SetQueueCap(ctx, "admin", 0); err != nil {
						t.Fatal(err)
					}
					if _, err := f.store.DrainDeferredBranch(ctx); err != nil {
						t.Fatal(err)
					}
				}
				run, err := f.store.ResolveRun(ctx, f.repo.ID, sha)
				if err != nil || !strings.Contains(run.Reason, "branch CI: park policy unavailable") || !strings.Contains(run.Reason, cause) {
					t.Fatalf("run lost fallback cause: %+v err=%v", run, err)
				}
				// Once accepted, a later policy recovery must not change this
				// receive into a park operation or strand its outbox replay.
				g.policy, g.policyErr = "", nil
				replay, err := p.Branch(ctx, push)
				if err != nil || !replay.Duplicate || replay.Run == nil || replay.Run.ID != run.ID || replay.Run.Reason != run.Reason || len(g.mirrored) != 0 {
					t.Fatalf("queued replay changed decision: %+v mirrored=%v err=%v", replay, g.mirrored, err)
				}
			})
		}
	}
}

func TestParkReceiptReplaySurvivesPolicyOutage(t *testing.T) {
	f := newControlFixture(t)
	sha := strings.Repeat("a", 40)
	g := &parkPushGit{stubPushGit: &stubPushGit{peeled: gitcache.PeeledObject{ObjectSHA: sha, CommitSHA: sha}}}
	p, err := NewPushIngestor(PushConfig{Repositories: f.store, Discoverer: stubRepositoryDiscoverer{}, Git: g, Receives: f.store, Branches: f.scheduler, Auditor: f.store})
	if err != nil {
		t.Fatal(err)
	}
	push := BranchPush{EventID: "park", Repository: f.repo.Name, Branch: "park/review", NewOID: sha, Actor: "dev"}
	if result, err := p.Branch(t.Context(), push); err != nil || result.Run != nil {
		t.Fatalf("park result=%+v err=%v", result, err)
	}
	g.policyErr = context.DeadlineExceeded
	replay, err := p.Branch(t.Context(), push)
	if err != nil || !replay.Duplicate || replay.Run != nil || len(g.mirrored) != 1 {
		t.Fatalf("park replay changed decision: %+v mirrored=%v err=%v", replay, g.mirrored, err)
	}
	push.Actor = "different actor"
	if _, err = p.Branch(t.Context(), push); err == nil {
		t.Fatal("reused receive identity accepted a different actor")
	}
}

func TestParkFallbackUsesExistingDisplayReason(t *testing.T) {
	cause := "branch CI: park policy unavailable (invalid promotion policy)"
	for _, status := range []model.RunStatus{model.RunQueued, model.RunRunning, model.RunPassed, model.RunFailed} {
		run := presentationRun(model.Run{Status: status, Reason: cause})
		if strings.Contains(run.DisplayReason, cause) == (run.DisplayStatus == model.DisplayQueued) {
			t.Errorf("status=%s lost visible fallback: %q", status, run.DisplayReason)
		}
	}
	run := presentationRun(model.Run{Status: model.RunQueued, Reason: cause})
	applyQueuedDisplay(&run, &model.RunScheduling{State: "queued", QueuePosition: 3}, "")
	if run.DisplayReason != "position 3" || run.Reason != cause {
		t.Errorf("queue observation hid fallback or duplicated it: %q", run.DisplayReason)
	}
	run = presentationRun(model.Run{Status: model.RunRunning, Reason: cause})
	applyExecutionDisplay(&run, &model.RunScheduling{Execution: &model.ExecutionObservation{State: "deadline_elapsed", Message: "awaiting finalization"}})
	if !strings.Contains(run.DisplayReason, "awaiting finalization") || strings.Count(run.DisplayReason, cause) != 1 {
		t.Errorf("execution observation hid fallback or duplicated it: %q", run.DisplayReason)
	}
	run = presentationRun(model.Run{Status: model.RunRunning, Reason: cause + "; " + store.PolicyRefreshWaitingReason})
	if strings.Contains(run.DisplayReason, cause) || strings.Contains(run.DisplayReason, store.PolicyRefreshWaitingReason) || !strings.Contains(run.Reason, cause) {
		t.Errorf("fallback copied a later policy-wait annotation: %q", run.DisplayReason)
	}
	f := newControlFixture(t)
	queued, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "scoped-fallback", Repository: f.repo, Branch: "park/review", SHA: strings.Repeat("a", 40), Actor: "dev", Reason: "branch run deferred: queue at 17/19; " + cause})
	if err != nil {
		t.Fatal(err)
	}
	value, err := f.api(t).Runs(t.Context(), api.Actor{Identity: "scoped", Scope: []string{"codeberg"}}, api.RunFilter{Ref: queued.Ref})
	shown, ok := value.([]model.Run)
	if err != nil || !ok || len(shown) != 1 {
		t.Fatalf("scoped runs: %+v %v", value, err)
	}
	if strings.Contains(shown[0].DisplayReason, cause) || !strings.Contains(shown[0].Reason, cause) || strings.Contains(shown[0].Reason, "17/19") || strings.Contains(shown[0].DisplayReason, "17/19") {
		t.Fatalf("scoped fallback lost reason or exposed global counts: %+v", shown[0])
	}
}

func TestParkFallbackReasonIsBoundedAndPrintable(t *testing.T) {
	sha := strings.Repeat("a", 40)
	g := &parkPushGit{stubPushGit: &stubPushGit{peeled: gitcache.PeeledObject{ObjectSHA: sha, CommitSHA: sha}}, policyErr: fmt.Errorf("invalid\x1b]52;payload\a\n%s", strings.Repeat("x", 1000))}
	queue := &stubCIQueue{}
	p, err := NewPushIngestor(PushConfig{Repositories: stubPushRepositories{repository: model.Repository{ID: 1, Name: "repo", DefaultBranch: "main"}}, Discoverer: stubRepositoryDiscoverer{}, Git: g, Receives: &stubReceiveRecorder{}, Branches: queue})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Branch(t.Context(), BranchPush{EventID: "fallback", Repository: "repo", Branch: "park/review", NewOID: sha, Actor: "dev"}); err != nil {
		t.Fatal(err)
	}
	if len(queue.requests) != 1 {
		t.Fatalf("fallback did not enqueue: %+v", queue.requests)
	}
	reason := queue.requests[0].Reason
	if len([]rune(reason)) > 600 || !strings.HasPrefix(reason, "branch CI: park policy unavailable (") {
		t.Fatalf("unbounded or missing fallback: %q", reason)
	}
	for _, r := range reason {
		if !unicode.IsPrint(r) {
			t.Fatalf("fallback contains terminal control: %q", reason)
		}
	}
	run := presentationRun(model.Run{Status: model.RunQueued, Reason: reason})
	if strings.Contains(run.DisplayReason, "branch CI: park policy unavailable") || run.Reason != reason {
		t.Fatalf("display lost or exceeded fallback bound: %q", run.DisplayReason)
	}
}
