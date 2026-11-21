package contracttest

import (
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func checkMixedReceive(t *testing.T, f RunFixture) {
	old := acceptOneRun(t, f, RunCase{ID: "old"})
	b := batchFor(t, f, RunCase{ID: "mixed", Ref: "refs/heads/dev"})
	req := b.Runs[0].Request.Clone()
	req.Ref = old.Ref
	req.PreviousSHA = old.ObjectSHA
	req.ObjectSHA = ""
	req.CommitSHA = nil
	p := model.Publication{ID: "delete", Repo: req.Repo, Ref: req.Ref, Actor: req.Actor.Identity, PreviousSHA: new(req.PreviousSHA)}
	b.Deletions = []stateport.BranchDeletion{{Request: req, Publication: p}}
	checkForeignDeletion(t, f, b)
	checkMixedRollback(t, f, b, old)
	ch := f.Port.Changes()
	recorded, err := f.Port.AcceptBatch(t.Context(), b)
	rows, fresh := recorded.Runs, recorded.Fresh
	mustRun(t, err)
	if !fresh || len(rows) != 1 {
		t.Fatal("mixed receive missing admission")
	}
	changedSignal(t, ch)
	got, err := f.Port.Run(t.Context(), old.ID)
	mustRun(t, err)
	if got.Status != stepmodel.RunCancelled {
		t.Fatal("mixed deletion did not cancel old branch")
	}
	pub, err := f.Publication(t.Context(), p.ID)
	mustRun(t, err)
	if pub.RunID != "" || pub.PreviousSHA == nil || *pub.PreviousSHA != old.ObjectSHA {
		t.Fatal("deletion publication lost predecessor", pub)
	}
	checkDeletionAudit(t, f, req)
	head := len(f.Audit(t))
	ch = f.Port.Changes()
	replayed, err := f.Port.AcceptBatch(t.Context(), b)
	fresh = replayed.Fresh
	mustRun(t, err)
	if fresh || len(f.Audit(t)) != head {
		t.Fatal("mixed receipt replay repeated facts")
	}
	unchangedSignal(t, ch)
}

func checkForeignDeletion(t *testing.T, f RunFixture, b stateport.BatchAcceptance) {
	t.Helper()
	f.Repository(t, "origin/team/other")
	bad := b
	bad.Deletions = append([]stateport.BranchDeletion(nil), b.Deletions...)
	bad.Deletions[0].Request.Repo = "origin/team/other"
	bad.Deletions[0].Publication.Repo = "origin/team/other"
	head := len(f.Audit(t))
	ch := f.Port.Changes()
	result, err := f.Port.AcceptBatch(t.Context(), bad)
	runCode(t, err, publicerr.InvalidInput)
	if result.Fresh || result.Runs != nil || len(f.Audit(t)) != head {
		t.Fatal("foreign deletion changed batch", result)
	}
	unchangedSignal(t, ch)
	_, err = f.Port.AcceptedRuns(t.Context(), b.ReceiveID)
	runCode(t, err, publicerr.NotFound)
	_, err = f.Publication(t.Context(), b.Deletions[0].Publication.ID)
	runCode(t, err, publicerr.NotFound)
}

func checkMixedRollback(t *testing.T, f RunFixture, b stateport.BatchAcceptance, old stateport.Run) {
	bad := b
	bad.Runs = append([]stateport.Acceptance(nil), b.Runs...)
	duplicate := batchFor(t, f, RunCase{ID: old.ID, Ref: "refs/heads/dev"}).Runs[0]
	duplicate.Request.ReceiveID = b.ReceiveID
	bad.Runs = append(bad.Runs, duplicate)
	ch := f.Port.Changes()
	head := len(f.Audit(t))
	failed, err := f.Port.AcceptBatch(t.Context(), bad)
	if err == nil || failed.Fresh || failed.Runs != nil {
		t.Fatal("failed batch returned accepted work", failed, err)
	}
	unchangedSignal(t, ch)
	got, err := f.Port.Run(t.Context(), old.ID)
	mustRun(t, err)
	if got.Status != old.Status || got.Revision != old.Revision || len(f.Audit(t)) != head {
		t.Fatal("failed mixed receive retained deletion")
	}
	_, err = f.Publication(t.Context(), b.Deletions[0].Publication.ID)
	runCode(t, err, publicerr.NotFound)
	_, err = f.Port.AcceptedRuns(t.Context(), b.ReceiveID)
	runCode(t, err, publicerr.NotFound)
}

func checkDeletionAudit(t *testing.T, f RunFixture, req model.CIRequest) {
	t.Helper()
	found := false
	for _, row := range f.Audit(t) {
		if row.Action == "branch.delete" && row.ResourceID == req.Repo && row.Actor == req.Actor.Identity {
			found = true
		}
	}
	if !found {
		t.Fatal("deletion lost audit attribution")
	}
}
