package store

import (
	"context"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestPublicationStateReadsOwnersFromOneCommittedSnapshot(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "feature/state", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	pub, err := s.BeginPublication(t.Context(), model.PublicationSpec{RepoID: repo.ID, RunID: run.ID, RefKind: run.RefKind, Ref: run.Ref, ResultSHA: run.SHA, Actor: run.Actor})
	if err != nil {
		t.Fatal(err)
	}
	reader, ok := any(s).(interface {
		PublicationState(context.Context, string) (model.PublicationFinalization, error)
	})
	if !ok {
		t.Fatal("publication and owner have no atomic status reader")
	}
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE runs SET status='passed' WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	state, err := reader.PublicationState(t.Context(), pub.ID)
	if err != nil || state.Publication.Status != model.PublicationPending || state.Run.Status != model.RunRunning {
		t.Fatalf("saw uncommitted owner: %+v %v", state, err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinalizePublication(t.Context(), pub.ID, model.PublicationDelivered, ""); err != nil {
		t.Fatal(err)
	}
	state, err = reader.PublicationState(t.Context(), pub.ID)
	if err != nil || state.Publication.Status != model.PublicationDelivered || state.Run.Status != model.RunPassed {
		t.Fatalf("completion snapshot: %+v %v", state, err)
	}
}
