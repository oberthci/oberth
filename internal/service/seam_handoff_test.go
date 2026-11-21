package service_test

import (
	"context"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/service"
	ports "github.com/oberthci/oberth/internal/service/contracttest"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"testing"
)

var (
	_ service.AuthorityStore = ports.AuthorityFuncs{}
	_ service.Git            = ports.GitFuncs{}
	_ service.MovingGit      = ports.MovingGitFuncs{}
	_ service.HoldExec       = ports.HoldExecFuncs{}
	_ service.Exec           = ports.ExecFuncs{}
	_ service.Logs           = ports.LogsFuncs{}
)

func TestPreparedRerunHasStableActionID(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunFailed)
	ids := []string{}
	f.starter.Start = func(_ context.Context, q service.StartRequest) (apicontract.Receipt, error) {
		ids = append(ids, q.Request.ReceiveID)
		return apicontract.Receipt{ID: "recorded-child"}, nil
	}
	call, e := f.s.PrepareRunRerun(t.Context(), workerCaller(), apicontract.RunRerunArgs{ID: runID})
	if e != nil {
		t.Fatal(e)
	}
	for range 2 {
		if _, e = call(t.Context()); e != nil {
			t.Fatal(e)
		}
	}
	if len(ids) != 2 || ids[0] == "" || ids[0] != ids[1] {
		t.Fatal("observation changed action identity", ids)
	}
}
func TestDecideSuppliesCanonicalDeletion(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	q := update()
	q.ObjectSHA = ""
	q.CommitSHA = ""
	q.PreviousSHA = head
	d, e := f.s.Decide(t.Context(), q)
	if e != nil || d.Deletion == nil {
		t.Fatal(d, e)
	}
	again, e := f.s.Decide(t.Context(), q)
	if e != nil {
		t.Fatal(e)
	}
	assertDeletionIntent(t, d, again, q)
	b := stateport.BatchAcceptance{ReceiveID: q.ID, Repository: repository, Actor: caller(), Deletions: []stateport.BranchDeletion{*d.Deletion}}
	if _, e = f.s.Accept(t.Context(), b); e != nil {
		t.Fatal(e)
	}
	got, e := f.c.Runs.Run(t.Context(), r.ID)
	if e != nil || got.Status != stepmodel.RunCancelled {
		t.Fatal("canonical decision did not stop branch", got, e)
	}
}

func assertDeletionIntent(t *testing.T, d, again service.Decision, q service.RefUpdate) {
	t.Helper()
	p := d.Deletion.Publication
	if p.ID == "" || p.ID != again.Deletion.Publication.ID || p.Repo != repository || p.Ref != q.Ref || p.Actor != caller().Identity || p.PreviousSHA == nil || *p.PreviousSHA != head {
		t.Fatal("incomplete or unstable deletion", p)
	}
}
