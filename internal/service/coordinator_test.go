package service_test

import (
	"context"
	"errors"
	statefake "github.com/oberthci/oberth/internal/stateport/fake"
	"reflect"
	"testing"
	"testing/synctest"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestDroppedPromotionControlsAreAbsent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		surface reflect.Type
		fields  []string
	}{
		{reflect.TypeFor[stateport.PublicationIntent](), []string{"GrantIDs"}},
		{reflect.TypeFor[model.Promotion](), []string{"Sources", "GrantID", "PruneSource", "TagAfter", "Force"}},
		{reflect.TypeFor[apicontract.RunRerunArgs](), []string{"Force"}},
		{reflect.TypeFor[apicontract.PromoteArgs](), []string{"Sources", "SkipConflicting", "PruneSource", "TagAfter"}},
	} {
		for _, name := range tc.fields {
			if _, ok := tc.surface.FieldByName(name); ok {
				t.Errorf("%s carries dropped %s", tc.surface, name)
			}
		}
	}
}

func TestCancelAllowsNativeProgressAfterPreparation(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	call, err := f.s.PrepareCancel(t.Context(), caller(), actionInput{ID: runID, Reason: "stop"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.native.Progress(t.Context(), statefake.NativeProgress{ID: runID, Revision: 1, Status: stepmodel.RunRunning, Detail: "new progress"}); err != nil {
		t.Fatal(err)
	}
	f.exec.CancelFunc = func(context.Context, stateport.Run, bool) error {
		return nil
	}
	_, err = call(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	stops, _ := f.store.native.Obligations(t.Context(), runID)
	if len(stops) != 1 || len(f.store.native.Audits()) != 1 {
		t.Fatal("native observation defeated cancellation")
	}
}
func TestCancelRejectsFinishAfterPreparation(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	call, err := f.s.PrepareCancel(t.Context(), caller(), actionInput{ID: runID})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.Finished(t.Context(), service.FinishedRun{RunID: runID, Status: stepmodel.RunPassed}); err != nil {
		t.Fatal(err)
	}
	f.exec.CancelFunc = func(context.Context, stateport.Run, bool) error {
		t.Fatal("finished run received native cancellation")
		return nil
	}
	_, err = call(t.Context())
	requireCode(t, err, publicerr.Conflict)
}
func deletion() stateport.BranchDeletion {
	r := model.CIRequest{ReceiveID: "deletion-1", Repo: repository, Ref: "refs/heads/topic", PreviousSHA: head, Actor: caller(), Trigger: model.TriggerBranch}
	return stateport.BranchDeletion{Request: r, Publication: model.Publication{ID: "deletion-publication", Repo: r.Repo, Ref: r.Ref, PreviousSHA: new(head), Actor: r.Actor.Identity, Status: model.PublicationPending}}
}
func TestReceivedDeletionIsOneAtomicFact(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	other := r.Clone()
	other.ID = "bbbbbbbb000000000000000000000002"
	other.Ref = "refs/heads/other"
	f.store.native.SeedRun(other)
	passed := r.Clone()
	passed.ID = "cccccccc000000000000000000000003"
	passed.Status = stepmodel.RunPassed
	f.store.native.SeedRun(passed)
	before := f.store.Calls()
	input := deletion()
	if err := recordDeletion(t, f, input); err != nil {
		t.Fatal(err)
	}
	checkDeletedRun(t, f, r)
	checkUnchangedRuns(t, f, []stateport.Run{other, passed})
	checkDeletionPublication(t, f)
	checkDeletionCalls(t, before, f.store.Calls())
	audits := len(f.store.native.Audits())
	if err := recordDeletion(t, f, input); err != nil {
		t.Fatal(err)
	}
	if len(f.store.native.Audits()) != audits || len(f.store.native.PublicationValues()) != 1 {
		t.Fatal("replay duplicated facts")
	}
}
func TestDeletionFailureRollsBackAndRejectsRunBinding(t *testing.T) {
	t.Parallel()
	for _, which := range []string{"event", "run", "fence"} {
		f := setup(t)
		f.seed(stepmodel.RunRunning)
		input := deletion()
		var events []stateport.EventWrite
		switch which {
		case "event":
			events = []stateport.EventWrite{{Dedupe: "bad"}}
		case "run":
			input.Publication.RunID = runID
		case "fence":
			input.Publication.PreviousSHA = nil
		}
		if err := recordDeletion(t, f, input, events...); err == nil {
			t.Fatal("invalid deletion accepted", which)
		}
		r, _ := f.store.Run(t.Context(), runID)
		if r.Status != stepmodel.RunRunning || len(f.store.native.PublicationValues()) != 0 || len(f.store.native.Audits()) != 0 {
			t.Fatal("partial deletion", which)
		}
	}
}
func TestDeletionPreservesStrongerStopAndReplayBinding(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	for range 2 {
		if _, err := f.s.Cancel(t.Context(), caller(), actionInput{ID: runID}); err != nil {
			t.Fatal(err)
		}
	}
	if err := recordDeletion(t, f, deletion()); err != nil {
		t.Fatal(err)
	}
	stops, _ := f.store.native.Obligations(t.Context(), runID)
	if stops[0].Value != "Terminate" {
		t.Fatal("deletion downgraded termination")
	}
	changed := deletion()
	changed.Publication.ID = "different-publication"
	requireCode(t, recordDeletion(t, f, changed), publicerr.Conflict)
}
func TestDefaultBranchComesFromFreshSource(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.source.Default = func(context.Context, string) (string, error) { return "refs/heads/trunk", nil }
	e := update()
	e.Ref = "refs/heads/main"
	if _, err := f.s.Decide(t.Context(), e); err != nil {
		t.Fatal("invented stored main", err)
	}
	e.Ref = "refs/heads/trunk"
	_, err := f.s.Decide(t.Context(), e)
	requireCode(t, err, publicerr.Forbidden)
	f.exec.InspectFunc = func(_ context.Context, q service.CheckRequest) (apicontract.RunCheckResult, error) {
		if q.Request.Ref != "refs/heads/trunk" {
			t.Fatal("check used stored default")
		}
		return apicontract.RunCheckResult{}, nil
	}
	if _, err := f.s.Check(t.Context(), caller(), service.CheckRequest{Request: model.CIRequest{Repo: repository}}); err != nil {
		t.Fatal(err)
	}
}
func TestDefaultSourceFailureNeverGuesses(t *testing.T) {
	t.Parallel()
	f := setup(t)
	failed := errors.New("source unavailable")
	f.source.Default = func(context.Context, string) (string, error) { return "", failed }
	_, err := f.s.Decide(t.Context(), update())
	if !errors.Is(err, failed) {
		t.Fatal(err)
	}
	_, err = f.s.Check(t.Context(), caller(), service.CheckRequest{Request: model.CIRequest{Repo: repository}})
	if !errors.Is(err, failed) {
		t.Fatal(err)
	}
	repos, err := f.s.Repos(t.Context(), caller())
	if err != nil || repos[0].DefaultBranch != "" || repos[0].BranchCount != nil {
		t.Fatalf("invented source metadata %+v %v", repos, err)
	}
}
func TestServiceHasNoLogicalProgressWriter(t *testing.T) {
	t.Parallel()
	surface := reflect.TypeFor[*service.Service]()
	for _, name := range []string{"Observe", "Observed", "BeginEvidence", "PutNodes", "CompleteEvidence"} {
		if _, ok := surface.MethodByName(name); ok {
			t.Fatal("service owns native progress", name)
		}
	}
}

func checkDeletedRun(t *testing.T, f *fixture, r stateport.Run) {
	t.Helper()
	got, _ := f.store.Run(t.Context(), r.ID)
	if got.Status != stepmodel.RunCancelled || got.Revision != r.Revision+1 {
		t.Fatalf("deletion run %+v", got)
	}
	stops, _ := f.store.native.Obligations(t.Context(), r.ID)
	if len(stops) != 1 || stops[0].Value != "Stop" {
		t.Fatalf("stop intent %+v", stops)
	}
}
func checkUnchangedRuns(t *testing.T, f *fixture, runs []stateport.Run) {
	t.Helper()
	for _, r := range runs {
		got, _ := f.store.Run(t.Context(), r.ID)
		if got.Status != r.Status {
			t.Fatal("deletion changed unrelated or terminal run")
		}
	}
}
func checkDeletionPublication(t *testing.T, f *fixture) {
	t.Helper()
	p := f.store.native.PublicationValues()
	if len(p) != 1 || p[0].RunID != "" || p[0].ObjectSHA != "" || p[0].CommitSHA != nil || p[0].PreviousSHA == nil {
		t.Fatalf("publication %+v", p)
	}
}
func checkDeletionCalls(t *testing.T, before, after map[string]int) {
	t.Helper()
	if after["DeleteBranch"] != before["DeleteBranch"] || after["AcceptBatch"]-before["AcceptBatch"] != 1 || after["Finish"] != before["Finish"] || after["Cancel"] != before["Cancel"] {
		t.Fatal("deletion fanned out", before, after)
	}
}
func TestRepositoryDefaultAndCountUseSource(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.source.Default = func(context.Context, string) (string, error) { return "refs/heads/trunk", nil }
	f.git.Refs = []gitport.Ref{{Name: "refs/heads/trunk"}, {Name: "refs/heads/main"}, {Name: "refs/heads/topic"}}
	rows, err := f.s.Repos(t.Context(), caller())
	if err != nil || rows[0].DefaultBranch != "refs/heads/trunk" || rows[0].BranchCount == nil || *rows[0].BranchCount != 2 {
		t.Fatalf("source metadata %+v %v", rows, err)
	}
}
func TestDeletionHiddenScopeAndWriteFailure(t *testing.T) {
	t.Parallel()
	f := setup(t)
	input := deletion()
	input.Request.Repo = "private/team/hidden"
	input.Publication.Repo = input.Request.Repo
	e := update()
	e.Repo, e.ObjectSHA, e.CommitSHA = input.Request.Repo, "", ""
	_, err := f.s.Decide(t.Context(), e)
	requireCode(t, err, publicerr.NotFound)
	if f.store.Calls()["AcceptBatch"] != 0 {
		t.Fatal("hidden deletion reached store")
	}
	f.seed(stepmodel.RunRunning)
	failure := errors.New("write unavailable")
	f.store.WriteError = failure
	if err := recordDeletion(t, f, deletion()); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	r, _ := f.store.Run(t.Context(), runID)
	if r.Status != stepmodel.RunRunning || len(f.store.native.PublicationValues()) != 0 {
		t.Fatal("partial failed deletion")
	}
}
func TestDirectNativeWriteWakesBlockedWaiter(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := setup(t)
		f.seed(stepmodel.RunRunning)
		call, err := f.s.PrepareRunWait(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.RunWaitArgs{ID: runID, Until: "any"})
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan waitOutcome, 1)
		go func() { out, err := call(t.Context()); done <- waitOutcome{out, err} }()
		synctest.Wait()
		if err := f.store.native.Progress(t.Context(), statefake.NativeProgress{ID: runID, Revision: 1, Status: stepmodel.RunRunning, Detail: "native changed"}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		select {
		case got := <-done:
			if got.err != nil || got.result.Run == nil || got.result.Run.Detail != "native detail restricted" || got.result.TimedOut {
				t.Fatalf("native wake %+v", got)
			}
		default:
			t.Fatal("direct native write did not wake waiter")
		}
	})
}

func recordDeletion(t *testing.T, f *fixture, d stateport.BranchDeletion, events ...stateport.EventWrite) error {
	t.Helper()
	_, err := f.s.Accept(t.Context(), stateport.BatchAcceptance{ReceiveID: d.Request.ReceiveID, Repository: d.Request.Repo, Actor: d.Request.Actor, Deletions: []stateport.BranchDeletion{d}, Events: events})
	return err
}
