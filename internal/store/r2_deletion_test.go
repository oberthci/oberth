package store

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestDeletionCommitsCancellationPublicationAndReceiptTogether(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, input := accepted(t, s, "deleted")
	input.Request.ReceiveID = "deletion"
	input.Request.PreviousSHA = r.ObjectSHA
	deletion := deletionForTest(input.Request)
	_, err := s.Cancel(t.Context(), stateport.Action{Actor: "operator"}, stateport.RunCancellation{ID: r.ID, Revision: r.Revision, Terminate: true})
	requireOK(t, err)
	event := atomicEvent(s, "deletion")
	requireOK(t, recordDeletion(t, s, deletion, event))
	got, err := s.Run(t.Context(), r.ID)
	requireOK(t, err)
	if got.Status != stepmodel.RunCancelled {
		t.Fatal("branch deletion left run active")
	}
	obligations, err := s.Obligations(t.Context(), r.ID)
	requireOK(t, err)
	if len(obligations) != 1 || obligations[0].Kind != "stop" || obligations[0].Value != "Terminate" {
		t.Fatal("deletion lost exact stop intent", obligations)
	}
	p, err := s.Publication(t.Context(), deletion.Publication.ID)
	requireOK(t, err)
	if p.Status != model.PublicationPending || p.ObjectSHA != "" || p.RunID != "" || p.PreviousSHA == nil || *p.PreviousSHA != r.ObjectSHA {
		t.Fatal("deletion intent lost its exact fence", p)
	}
	rows, err := s.AcceptedRuns(t.Context(), deletion.Request.ReceiveID)
	requireOK(t, err)
	if len(rows) != 0 {
		t.Fatal("deletion receipt invented runs")
	}
	before := databaseContents(t, s.readers)
	requireOK(t, recordDeletion(t, s, deletion, event))
	if before != databaseContents(t, s.readers) {
		t.Fatal("deletion replay repeated work")
	}
	// Arrange a settled obligation to isolate publication protection on archive.
	execute(t, s.writer, `UPDATE obligations SET value='' WHERE run_id='deleted'`)
	requireCode(t, s.ArchiveRepository(t.Context(), stateport.Action{Actor: "operator"}, r.RepositoryID), publicerr.Conflict)
	_, err = s.StartPublicationAttempt(t.Context(), stateport.Action{Actor: "operator"}, stateport.PublicationAttempt{ID: p.ID})
	requireOK(t, err)
	requireOK(t, s.SettlePublication(t.Context(), stateport.Action{Actor: "operator"}, stateport.PublicationResult{ID: p.ID, Attempt: 1, Delivered: true}))
	requireOK(t, s.ArchiveRepository(t.Context(), stateport.Action{Actor: "operator"}, r.RepositoryID))
}

func TestDeletionRejectsChangedPublicationAndUnboundFence(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	_, input := accepted(t, s, "deleted")
	input.Request.ReceiveID = "deletion"
	deletion := deletionForTest(input.Request)
	requireOK(t, recordDeletion(t, s, deletion))
	changed := deletion
	changed.Publication.ID = "other-publication"
	requireCode(t, recordDeletion(t, s, changed), publicerr.Conflict)
	changed = deletion
	changed.Request.ReceiveID = "new-receive"
	changed.Publication.PreviousSHA = nil
	requireCode(t, recordDeletion(t, s, changed), publicerr.InvalidInput)
	changed.Publication.PreviousSHA = new("different")
	requireCode(t, recordDeletion(t, s, changed), publicerr.InvalidInput)
}

func TestDeletionRequiresExactReceiptBinding(t *testing.T) {
	t.Parallel()
	for _, change := range []struct {
		name string
		edit func(*stateport.BranchDeletion)
	}{
		{"id", func(d *stateport.BranchDeletion) { d.Publication.ID = "" }},
		{"actor", func(d *stateport.BranchDeletion) { d.Publication.Actor = "other" }},
		{"repository", func(d *stateport.BranchDeletion) { d.Publication.Repo = "origin/team/other" }},
		{"ref", func(d *stateport.BranchDeletion) { d.Publication.Ref = "refs/heads/other" }},
		{"unknown predecessor", func(d *stateport.BranchDeletion) { d.Publication.PreviousSHA = nil }},
		{"different predecessor", func(d *stateport.BranchDeletion) { d.Publication.PreviousSHA = new("other") }},
		{"object", func(d *stateport.BranchDeletion) { d.Publication.ObjectSHA = "commit" }},
		{"run", func(d *stateport.BranchDeletion) { d.Publication.RunID = "deleted" }},
		{"promotion", func(d *stateport.BranchDeletion) { d.Publication.PromotionID = "promotion" }},
		{"commit", func(d *stateport.BranchDeletion) { d.Publication.CommitSHA = new("commit") }},
	} {
		t.Run(change.name, func(t *testing.T) {
			s := testStore(t)
			_, input := accepted(t, s, "deleted")
			input.Request.ReceiveID = "delete"
			deletion := deletionForTest(input.Request)
			change.edit(&deletion)
			before := databaseContents(t, s.readers)
			requireCode(t, recordDeletion(t, s, deletion), publicerr.InvalidInput)
			if before != databaseContents(t, s.readers) {
				t.Fatal("invalid deletion wrote facts")
			}
		})
	}
}

func TestDeletionPublicationSurvivesVerifiedRestoreWithoutRun(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	_, input := accepted(t, s, "deleted")
	input.Request.ReceiveID = "delete"
	deletion := deletionForTest(input.Request)
	requireOK(t, recordDeletion(t, s, deletion))
	var archive bytes.Buffer
	_, err := s.Snapshot(t.Context(), backupOptions(s), &archive)
	requireOK(t, err)
	destination := filepath.Join(t.TempDir(), "restored.sqlite")
	_, err = backupForTest(t, s).Restore(t.Context(), restoreRequest(s, destination), bytes.NewReader(archive.Bytes()))
	requireOK(t, err)
	restored, err := Open(t.Context(), destination, s.options)
	requireOK(t, err)
	defer func() { requireOK(t, restored.Close()) }()
	p, err := restored.Publication(t.Context(), deletion.Publication.ID)
	requireOK(t, err)
	if p.RunID != "" || p.ObjectSHA != "" || p.Status != model.PublicationPending {
		t.Fatal("run-less deletion did not restore", p)
	}
	before := databaseContents(t, restored.readers)
	requireOK(t, recordDeletion(t, restored, deletion))
	if before != databaseContents(t, restored.readers) {
		t.Fatal("restored deletion refired")
	}
}

func TestDeletionOnlyCancelsActiveRunsOnItsRef(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, input := accepted(t, s, "active-main")
	other := reacceptance(t, input, "active-other")
	view := other.Prepared.View()
	view.Ref = "refs/heads/other"
	other.Request.Ref = view.Ref
	other.Prepared = sealView(t, view)
	_, err := acceptForTest(t.Context(), s, other)
	requireOK(t, err)
	passed, _ := accepted(t, s, "passed-main")
	requireOK(t, s.Finish(t.Context(), stateport.Action{}, stateport.RunUpdate{ID: passed.ID, Revision: passed.Revision, Status: stepmodel.RunPassed}))
	input.Request.ReceiveID = "delete"
	requireOK(t, recordDeletion(t, s, deletionForTest(input.Request)))
	for id, want := range map[string]stepmodel.RunStatus{r.ID: stepmodel.RunCancelled, "active-other": stepmodel.RunQueued, passed.ID: stepmodel.RunPassed} {
		got, err := s.Run(t.Context(), id)
		requireOK(t, err)
		if got.Status != want {
			t.Fatal("deletion affected wrong work", id, got.Status, want)
		}
	}
}

func TestDeletedRefCanBeRecreatedAndDeletedAgain(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	_, first := accepted(t, s, "first")
	first.Request.ReceiveID = "first-deletion"
	deletion := deletionForTest(first.Request)
	requireOK(t, recordDeletion(t, s, deletion))
	_, err := s.StartPublicationAttempt(t.Context(), stateport.Action{Actor: "operator"}, stateport.PublicationAttempt{ID: deletion.Publication.ID})
	requireOK(t, err)
	requireOK(t, s.SettlePublication(t.Context(), stateport.Action{Actor: "operator"}, stateport.PublicationResult{ID: deletion.Publication.ID, Attempt: 1, Delivered: true}))
	r, second := accepted(t, s, "recreated")
	second.Request.ReceiveID = "second-deletion"
	next := deletionForTest(second.Request)
	requireOK(t, recordDeletion(t, s, next))
	got, err := s.Run(t.Context(), r.ID)
	requireOK(t, err)
	if got.Status != stepmodel.RunCancelled {
		t.Fatal("recreated branch remained active")
	}
	p, err := s.Publication(t.Context(), next.Publication.ID)
	requireOK(t, err)
	if p.Status != model.PublicationPending {
		t.Fatal("second deletion was not publishable", p)
	}
}
