package contracttest

import (
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func checkDeletion(t *testing.T, f RunsFixture) {
	t.Helper()
	row := acceptOne(t, f)
	f.Stop("Terminate")
	b := f.Acceptance
	b.ReceiveID = "delete"
	b.Runs = nil
	r := f.Acceptance.Runs[0].Request.Clone()
	r.ReceiveID = b.ReceiveID
	r.PreviousSHA = r.ObjectSHA
	r.ObjectSHA = ""
	r.CommitSHA = nil
	p := model.Publication{ID: "publication", Actor: r.Actor.Identity, Repo: r.Repo, Ref: r.Ref, PreviousSHA: new(r.PreviousSHA)}
	b.Deletions = []stateport.BranchDeletion{{Request: r, Publication: p}}
	recorded, err := f.Port.Accept(t.Context(), b)
	if err != nil || !recorded.Fresh || len(recorded.Runs) != 0 {
		t.Fatal("deletion-only batch refused", recorded.Runs, recorded.Fresh, err)
	}
	if _, err := f.Port.AcceptedRuns(t.Context(), b.ReceiveID); err != nil {
		t.Fatal("deletion receipt missing", err)
	}
	rows, err := f.Port.AcceptedRuns(t.Context(), f.Acceptance.ReceiveID)
	if err != nil || len(rows) != 1 || rows[0].Status != "cancelled" || rows[0].DisplayReason != stateport.ReasonBranchDeleted {
		t.Fatal("deletion did not cancel active run", rows, err)
	}
	stops, err := f.Port.Obligations(t.Context(), row.ID)
	if err != nil || len(stops) != 1 || stops[0].Value != "Terminate" {
		t.Fatal("deletion downgraded stop", stops, err)
	}
	checkDeletionReplay(t, f, b)
}
func checkDeletionReplay(t *testing.T, f RunsFixture, b stateport.BatchAcceptance) {
	t.Helper()
	if recorded, err := f.Port.Accept(t.Context(), b); err != nil || recorded.Fresh {
		t.Fatal("deletion replay repeated work", err)
	}
	b.Deletions = append([]stateport.BranchDeletion(nil), b.Deletions...)
	b.Deletions[0].Publication.PreviousSHA = new("")
	if _, err := f.Port.Accept(t.Context(), b); err == nil {
		t.Fatal("deletion replay changed predecessor")
	}
}

func checkDecidedAuthority(t *testing.T, f RunsFixture) {
	t.Helper()
	b := f.Acceptance
	b.Actor.Scope = []string{"foreign"}
	b.Runs = append([]stateport.Acceptance(nil), b.Runs...)
	for i := range b.Runs {
		b.Runs[i].Request.Actor = b.Actor.Clone()
	}
	recorded, err := f.Port.Accept(t.Context(), b)
	if err != nil || !recorded.Fresh || len(recorded.Runs) != len(b.Runs) {
		t.Fatal("decided batch rechecked scope", recorded, err)
	}
}
