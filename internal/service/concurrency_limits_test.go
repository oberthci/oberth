package service

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestConcurrencyGroupsConflictDuringVMReadmission(t *testing.T) {
	g := newWorkflowAdmissionForJobs(4)
	r := g.reserveRun(model.Run{ID: "vm", RepoID: 1, QueueSequence: 1})
	request := admissionRequest{RunID: "vm", RepoID: 1, Group: "state", GroupLimit: 2}
	release := acquireRequestWithinBudget(t, r, request)
	defer release()
	done := make(chan error, 1)
	go func() {
		for range 1000 {
			if err := r.parkVM(); err != nil {
				done <- err
				return
			}
			if _, err := r.acquireRequest(t.Context(), request); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for i := int64(2); i < 1002; i++ {
		id := fmt.Sprint(i)
		other := g.reserveRun(model.Run{ID: id, RepoID: 2, QueueSequence: i})
		if _, err := other.acquireRequest(t.Context(), admissionRequest{RunID: id, RepoID: 2, Group: "state", GroupLimit: 1}); err == nil || !strings.Contains(err.Error(), "limit 1 conflicts with limit 2") {
			t.Errorf("unexpected conflict: %v", err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type twoMemberGroupEngine struct{ *groupEngine }

func (engine twoMemberGroupEngine) PipelineConcurrencyContract(ctx context.Context, request WorkflowRequest) (string, int, error) {
	group, err := engine.PipelineConcurrencyGroup(ctx, request)
	return group, 2, err
}

func TestConcurrencyGroupsSchedulerPersistsAndSharesYAMLLimit(t *testing.T) {
	f := newGroupFixture(t, 4)
	f.scheduler.jobs = twoMemberGroupEngine{f.engine}
	f.scheduler.releaseJobs = twoMemberGroupEngine{f.engine}
	for _, ref := range []string{"first", "second", "release-1"} {
		f.engine.declare(ref, "state", pipeline.M)
	}
	first := f.branch(t, f.terraform, "first", "a")
	second := f.branch(t, f.exampleService, "second", "b")
	f.start(t)
	created := f.awaitCreated(t, 2)
	if !created[first.ID] || !created[second.ID] {
		t.Fatal("two group members did not start", created)
	}
	third := f.tag(t, f.terraform, "release-1", "c", "d")
	f.awaitParked(t, third, first.ID)
	f.assertNothingCreated(t)
	if got := f.run(t, third.ID); got.ConcurrencyLimit != 2 || got.ConcurrencyGroup != "state" {
		t.Fatal("YAML limit not persisted", got)
	}
	f.engine.finish(t, first.ID)
	if next := f.awaitCreated(t, 1); !next[third.ID] {
		t.Fatal("waiting release not admitted", next)
	}
	f.engine.finish(t, second.ID)
	f.engine.finish(t, third.ID)
	f.engine.assertNoViolations(t)
}

func TestConcurrencyGroupsShareLimitAcrossRepositoriesAndTriggers(t *testing.T) {
	g := newWorkflowAdmissionForJobs(4)
	s := &Scheduler{admission: g}
	first := g.reserveRun(model.Run{ID: "branch", RepoID: 1, QueueSequence: 1, RefKind: model.RefBranch})
	releaseFirst := acquireRequestWithinBudget(t, first, admissionRequest{RunID: "branch", RepoID: 1, Group: "state", GroupLimit: 2})
	second := g.reserveRun(model.Run{ID: "tag", RepoID: 2, QueueSequence: 2, RefKind: model.RefTag})
	releaseSecond := acquireRequestWithinBudget(t, second, admissionRequest{RunID: "tag", RepoID: 2, Group: "state", GroupLimit: 2})
	third := g.reserveRun(model.Run{ID: "promotion", RepoID: 3, QueueSequence: 3, Trigger: "promotion"})
	ready := make(chan func(), 1)
	go acquireRequestForTest(third, admissionRequest{RunID: "promotion", RepoID: 3, Group: "state", GroupLimit: 2}, ready)
	awaitWaiting(t, third)
	view := s.AdmissionSnapshot(3)
	if granted(g, third) || view.GroupLimit != 2 || len(view.GroupHolders) != 2 {
		t.Fatalf("limit not shared: %+v", view)
	}
	other := g.reserveRun(model.Run{ID: "other", RepoID: 4, QueueSequence: 4})
	releaseOther := acquireRequestWithinBudget(t, other, admissionRequest{RunID: "other", RepoID: 4})
	releaseFirst()
	releaseThird := awaitAcquired(t, ready)
	releaseThird()
	releaseSecond()
	releaseOther()
	if g.runs != 0 {
		t.Fatal("leaked workflow slots", g.runs)
	}
}

func TestConcurrencyGroupsRefuseConflictingLiveLimit(t *testing.T) {
	g := newWorkflowAdmissionForJobs(4)
	r := g.reserveRun(model.Run{ID: "first", RepoID: 1, QueueSequence: 1})
	release := acquireRequestWithinBudget(t, r, admissionRequest{RunID: "first", RepoID: 1, Group: "state", GroupLimit: 2})
	defer release()
	conflict := g.reserveRun(model.Run{ID: "second", RepoID: 2, QueueSequence: 2})
	if _, err := conflict.acquireRequest(t.Context(), admissionRequest{RunID: "second", RepoID: 2, Group: "state", GroupLimit: 1}); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatal("accepted conflicting group", err)
	}
}

func TestConcurrencyGroupsAdoptMultipleHolders(t *testing.T) {
	g := newWorkflowAdmissionForJobs(4)
	for i := int64(1); i <= 2; i++ {
		r := model.Run{ID: strings.Repeat(string(rune('a'+i)), 32), RepoID: i, QueueSequence: i, ConcurrencyGroup: "state", ConcurrencyLimit: 2}
		if _, err := g.adopt(r, pipeline.M, argoworkflow.VMIDemand{}); err != nil {
			t.Fatal(err)
		}
	}
	s := &Scheduler{admission: g}
	state := s.AdmissionState()
	if len(state.Groups) != 1 || state.Groups[0].Limit != 2 || len(state.Groups[0].Holders) != 2 {
		t.Fatalf("lost adopted holders: %+v", state.Groups)
	}
	if _, err := g.adopt(model.Run{ID: "excess", RepoID: 3, QueueSequence: 3, ConcurrencyGroup: "state", ConcurrencyLimit: 2}, pipeline.M, argoworkflow.VMIDemand{}); err == nil {
		t.Fatal("adopted more than an explicit YAML limit")
	}
}

func TestConcurrencyGroupsRemainBoundedByWorkflowCeiling(t *testing.T) {
	g := newWorkflowAdmissionForJobs(1)
	first := g.reserveRun(model.Run{ID: "first", QueueSequence: 1, RepoID: 1})
	release := acquireRequestWithinBudget(t, first, admissionRequest{RunID: "first", RepoID: 1, Group: "state", GroupLimit: 4})
	next := g.reserveRun(model.Run{ID: "second", QueueSequence: 2, RepoID: 2, Release: true, RefKind: model.RefTag})
	ready := make(chan func(), 1)
	go acquireRequestForTest(next, admissionRequest{RunID: "second", RepoID: 2, Group: "state", GroupLimit: 4}, ready)
	awaitWaiting(t, next)
	if granted(g, next) {
		t.Fatal("group limit bypassed workflow ceiling")
	}
	release()
	awaitAcquired(t, ready)()
}

func TestConcurrencyGroupsAdoptionPreservesLegacyOverLimitWork(t *testing.T) {
	g := newWorkflowAdmissionForJobs(4)
	var holders []*admissionReservation
	for i := int64(1); i <= 2; i++ {
		r, err := g.adopt(model.Run{ID: string(rune('a' + i)), RepoID: i, QueueSequence: i, ConcurrencyGroup: "state"}, pipeline.M, argoworkflow.VMIDemand{})
		if err != nil {
			t.Fatal(err)
		}
		holders = append(holders, r)
	}
	next := g.reserveRun(model.Run{ID: "next", RepoID: 3, QueueSequence: 3})
	ready := make(chan func(), 1)
	go acquireRequestForTest(next, admissionRequest{RunID: "next", RepoID: 3, Group: "state"}, ready)
	awaitWaiting(t, next)
	holders[0].release()
	if granted(g, next) {
		t.Fatal("legacy excess holder bypassed limit")
	}
	holders[1].release()
	awaitAcquired(t, ready)()
}
