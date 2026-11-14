package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
)

// Cancelled receipts remain replayable without hiding a recreated branch.
func TestDeletedReceiveReplayMustNotHideRecreatedRun(t *testing.T) {
	f := newControlFixture(t)
	ctx := t.Context()
	occupant, err := f.scheduler.recordCIForTest(ctx, CIRequest{EventID: "occupant", Repository: f.repo, Branch: "feature/occupant", SHA: strings.Repeat("b", 40), Actor: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	g := &parkPushGit{stubPushGit: &stubPushGit{peeled: gitcache.PeeledObject{ObjectSHA: sha, CommitSHA: sha}}}
	ingestor, err := NewPushIngestor(PushConfig{Repositories: f.store, Discoverer: stubRepositoryDiscoverer{}, Git: g, Receives: f.store, Branches: fixtureEnqueues{f.scheduler}, Auditor: f.store})
	if err != nil {
		t.Fatal(err)
	}
	push := BranchPush{EventID: "deferred", Repository: f.repo.Name, Branch: "feature/recreated", NewOID: sha, Actor: "dev"}
	deferred, err := ingestor.Branch(ctx, push)
	if err != nil || deferred.Run == nil {
		t.Fatalf("defer: %+v %v", deferred, err)
	}
	_, err = ingestor.Branch(ctx, BranchPush{EventID: "delete", Repository: f.repo.Name, Branch: push.Branch, OldOID: sha, NewOID: strings.Repeat("0", 40), Actor: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	push.EventID = "re-push"
	fresh, err := ingestor.Branch(ctx, push)
	if err != nil || fresh.Run == nil {
		t.Fatalf("re-push did not create run: %+v %v", fresh, err)
	}
	f.refs = stubRefResolver{branches: map[string]map[string]string{"codeberg/acme/oberth": {push.Branch: sha}}}
	control := f.api(t)
	args, err := json.Marshal(map[string]string{"repo": f.repo.Name, "ref": push.Branch})
	if err != nil {
		t.Fatal(err)
	}
	checkStatus := func(stage, expectedStatus string) {
		t.Helper()
		resolved, err := f.store.ResolveRun(ctx, f.repo.ID, sha)
		if err != nil || resolved.ID != fresh.Run.ID {
			t.Fatalf("actual run missing: %+v %v", resolved, err)
		}
		for _, actor := range []api.Actor{{Identity: "agent@host"}, {Identity: "scoped", Scope: []string{"codeberg"}}} {
			value, err := control.CallTool(ctx, actor, "status", args)
			if err != nil {
				t.Fatal(err)
			}
			shown := value.(StatusResponse)
			t.Logf("%s actor=%s actual_run=%s actual_status=%s shown_run=%q shown_status=%s shown_reason=%q", stage, actor.Identity, resolved.ID, resolved.Status, shown.RunID, shown.Status, shown.DisplayReason)
			if shown.RunID != fresh.Run.ID || shown.Status != expectedStatus {
				t.Errorf("%s: cancelled deferred receipt hides new run for %s", stage, actor.Identity)
			}
		}
	}
	checkStatus("new run preparing", "waiting")
	for _, id := range []string{occupant.ID, fresh.Run.ID} {
		claimed, err := f.store.StartRun(ctx, id)
		if err != nil || claimed.ID != id {
			t.Fatalf("claim: %+v %v", claimed, err)
		}
		if _, err := f.store.FinishRun(ctx, id, model.RunResult{Status: model.RunPassed, TestedSHA: claimed.SHA}); err != nil {
			t.Fatal(err)
		}
	}
	checkStatus("new run passed", "delivered")
	push.EventID = "deferred"
	replay, err := ingestor.Branch(ctx, push)
	if err != nil || !replay.Duplicate || replay.Run == nil || replay.Run.ID != deferred.Run.ID || !replay.Run.Cancelled() {
		t.Fatalf("cancelled receipt lost replay identity: %+v %v", replay, err)
	}
	checkStatus("after old receive replay", "delivered")
}
