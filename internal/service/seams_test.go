package service_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestAuthorityChangesAfterGateStillRecordsSealedBatch(t *testing.T) {
	t.Parallel()
	f := setup(t)
	decision, err := f.s.Decide(t.Context(), update())
	if err != nil {
		t.Fatal(err)
	}
	b := batch(t)
	b.Runs[0].Request = decision.Request
	f.store.Actors[caller().Fingerprint] = actor.Actor{Identity: "revoked"}
	f.store.PolicyError = errors.New("policy changed")
	f.archiveRepository(t, repository)
	before := f.store.Calls()
	f.source.Default = func(context.Context, string) (string, error) {
		t.Error("default read after seal")
		return "", errors.New("source changed")
	}
	result, err := f.s.Accept(t.Context(), b)
	rows, fresh := result.Runs, result.Fresh
	if err != nil || !fresh || len(rows) != 1 || rows[0].Admission.Digest != b.Runs[0].Prepared.View().Digest || rows[0].RepositoryID != 1 {
		t.Fatalf("recorded %+v %v %v", rows, fresh, err)
	}
	for _, name := range []string{"Current", "KeyPolicy", "RepositoryScope", "Repository", "Repositories"} {
		if f.store.Calls()[name] != before[name] {
			t.Fatalf("%s revalidated after seal", name)
		}
	}
	if f.store.Calls()["AcceptBatch"] != before["AcceptBatch"]+1 {
		t.Fatal("accept did not use one store fact")
	}
	if _, ok := reflect.TypeFor[*service.Service]().MethodByName("DeleteBranch"); ok {
		t.Fatal("separate receive deletion entry point remains")
	}
}

func TestMixedBatchPreservesRejectedAndSealedOutcomes(t *testing.T) {
	t.Parallel()
	f := setup(t)
	old := f.seed(stepmodel.RunRunning)
	b := batch(t)
	a := b.Runs[0]
	a.Request = a.Request.Clone()
	a.Request.Ref = "refs/heads/new"
	a.Prepared = ownerAdmission(t, "bbbbbbbb000000000000000000000002", a.Request)
	refused := a.Request.Clone()
	refused.Ref = "refs/heads/refused"
	r := &stateport.RejectedAdmission{ID: "cccccccc000000000000000000000003", Failure: stepmodel.FailureRecord{Class: stepmodel.FailureAuthor, Retry: stepmodel.RetryHint{Action: stepmodel.RetryRerun}}, Findings: []admissioncontract.Finding{{Path: "pipeline", Code: "invalid", Reason: "admission refused", Remedy: "Fix the pipeline."}}}
	b.Runs = []stateport.Acceptance{a, {Request: refused, Rejected: r}}
	d := deletion()
	d.Request.ReceiveID = b.ReceiveID
	b.Deletions = []stateport.BranchDeletion{d}
	before := f.store.Calls()
	result, err := f.s.Accept(t.Context(), b)
	rows, fresh := result.Runs, result.Fresh
	if err != nil || !fresh || len(rows) != 2 {
		t.Fatalf("mixed %+v %v %v", rows, fresh, err)
	}
	checkMixedOutcomes(t, rows, a, r)
	checkDeletedRun(t, f, old)
	checkDeletionPublication(t, f)
	checkDeletionCalls(t, before, f.store.Calls())
	checkMixedObservations(t, f, b)
	audits := len(f.store.native.Audits())
	ch := observeChanges(t, f.store)
	if result, err := f.s.Accept(t.Context(), b); err != nil || result.Fresh || len(f.store.native.Audits()) != audits {
		t.Fatal("mixed replay repeated facts", err)
	}
	select {
	case <-ch:
		t.Fatal("replay woke observers")
	default:
	}
	checkRejectedSnapshotIsDetached(t, f, b)
}

func checkMixedObservations(t *testing.T, f *fixture, b stateport.BatchAcceptance) {
	t.Helper()
	page, err := f.store.Events(t.Context(), stateport.EventFilter{Limit: 500})
	if err != nil || len(page.Events) != 3 {
		t.Fatalf("batch observations %+v %v", page, err)
	}
	terminal := 0
	for _, e := range page.Events {
		if e.Kind == string(model.EventRunFinished) {
			terminal++
		}
	}
	if terminal != 1 {
		t.Fatal("rejected run lacks its terminal observation")
	}
	found := false
	for _, a := range f.store.native.Audits() {
		if auditTargetMatches(a, b.Runs[0].Actions[0]) {
			found = true
		}
		if strings.Contains(a.Details, "fingerprint") {
			t.Fatal("credential identity leaked into request audit")
		}
	}
	if !found {
		t.Fatal("caller audit lost")
	}
}

func TestAcceptanceDoesNotMutateCallerBatch(t *testing.T) {
	t.Parallel()
	f := setup(t)
	b := batch(t)
	actions := make([]model.AuditAction, 2, 4)
	actions[0] = b.Runs[0].Actions[0]
	actions[1] = model.AuditAction{Action: "sentinel"}
	events := make([]stateport.EventWrite, 2, 4)
	events[0] = b.Runs[0].Events[0]
	events[1] = stateport.EventWrite{Dedupe: "sentinel"}
	b.Runs[0].Actions = actions[:1]
	b.Runs[0].Events = events[:1]
	if _, err := f.s.Accept(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	if len(b.Runs[0].Actions) != 1 || len(b.Runs[0].Events) != 1 || actions[1].Action != "sentinel" || events[1].Dedupe != "sentinel" {
		t.Fatal("acceptance mutated caller-owned slices")
	}
}

func TestRepositoryCataloguePagesRetainEveryLatestRun(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.store.native.SeedRepositories(nil)
	for i := range 202 {
		name := fmt.Sprintf("origin/team/repo-%03d", i)
		f.store.native.SeedRepository(stateport.Repository{ID: int64(i + 1), Upstream: "origin", Name: fmt.Sprint("repo-", i), Canonical: name})
		f.store.native.SeedRun(stateport.Run{RunSummary: stepmodel.RunSummary{ID: fmt.Sprintf("%032x", i+1), Repo: name, Ref: "refs/heads/main", Status: stepmodel.RunPassed}})
	}
	call, err := f.s.PrepareRepoList(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.RepoListArgs{})
	if err != nil {
		t.Fatal(err)
	}
	page, err := call(t.Context())
	if err != nil || len(page.Repositories) != 202 {
		t.Fatalf("catalogue %d %v", len(page.Repositories), err)
	}
	for _, r := range page.Repositories {
		if r.LatestRun == nil || r.LatestRun.Repo != r.Repo || !strings.HasPrefix(r.Repo, "origin/team/") {
			t.Fatal("canonical repository or latest run lost", r)
		}
	}
	calls := f.store.Calls()
	if calls["Repositories"] != 2 || calls["Runs"] != 2 {
		t.Fatal("catalogue or latest projections not paged", calls)
	}
}

func TestRunListGlobIncludesNestedBranches(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	r.Ref = "refs/heads/topic/nested"
	f.store.native.SeedRun(r)
	call, err := f.s.Service.PrepareRunList(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.RunListArgs{RefGlob: "refs/heads/*"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := call(t.Context())
	if err != nil || len(out.Runs) != 1 || out.Runs[0].Ref != r.Ref {
		t.Fatalf("whole-ref glob %+v %v", out, err)
	}
}

func checkMixedOutcomes(t *testing.T, rows []stateport.Run, a stateport.Acceptance, r *stateport.RejectedAdmission) {
	t.Helper()
	if rows[0].Admission.Digest != a.Prepared.View().Digest || !rows[0].HasAdmission || rows[1].HasAdmission || !reflect.DeepEqual(rows[1].Findings, r.Findings) || rows[1].ID != r.ID || rows[1].RepositoryID != 1 {
		t.Fatal("owner outcomes lost", rows)
	}
}

func TestUnscopedEmptyCredentialScopeListsCanonicalRepositories(t *testing.T) {
	t.Parallel()
	f := setup(t)
	a := admin()
	a.Scope = []string{}
	f.store.Actors[a.Fingerprint] = a
	call, err := f.s.PrepareRepoList(t.Context(), apicontract.Caller{Actor: a}, apicontract.RepoListArgs{})
	if err != nil {
		t.Fatal(err)
	}
	page, err := call(t.Context())
	if err != nil || len(page.Repositories) != 1 || page.Repositories[0].Repo != repository {
		t.Fatalf("empty credential scope %+v %v", page, err)
	}
}

func checkRejectedSnapshotIsDetached(t *testing.T, f *fixture, b stateport.BatchAcceptance) {
	t.Helper()
	r := b.Runs[1].Rejected
	reason := r.Findings[0].Reason
	r.Findings[0].Reason = "caller mutation"
	stored, err := f.store.Run(t.Context(), r.ID)
	if err != nil || stored.Findings[0].Reason != reason {
		t.Fatal("rejected findings alias caller", err)
	}
	_, err = f.s.Accept(t.Context(), b)
	requireCode(t, err, publicerr.Conflict)
}

func auditTargetMatches(a, b model.AuditAction) bool {
	return a.Actor == b.Actor && a.Action == b.Action && a.ResourceType == b.ResourceType && a.ResourceID == b.ResourceID && a.Details == b.Details
}
