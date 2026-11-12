package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestArchivedRepositoryNeverClaimsRestoredQueuedRun(t *testing.T) {
	s := nativeReceiveUpgradeStore(t)
	applyNativeReceiveUpgrade(t, s)
	repo := createRepo(t, s)
	ctx := t.Context()
	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "main", projectionSourceSHA))
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a historical/restored queued row beside immutable archive state.
	// Current archive correctly refuses to create this combination itself.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: "admin", Action: "repository.archive", ResourceType: "repository", ResourceID: fmt.Sprint(repo.ID), Details: `{}`}, unixNano(s.now()))
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, next := range []func() error{
		func() error { _, err := s.NextQueuedRun(ctx); return err },
		func() error { _, err := s.ClaimNextRun(ctx); return err },
	} {
		if err := next(); !errors.Is(err, ErrNotFound) {
			t.Fatal("archived run passed authorization predicate", err)
		}
	}
	kept, err := s.Run(ctx, run.ID)
	if err != nil || kept.Status != model.RunQueued || kept.StartedAt != nil || kept.WorkflowName != "" {
		t.Fatal("archive claim guard mutated/prepared the run", kept, err)
	}
	if _, err = s.SetRepositoryArchived(ctx, "admin", repo.Name, false); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimNextRun(ctx)
	if err != nil || claimed.ID != run.ID {
		t.Fatal("unarchive did not release authorization wait", claimed, err)
	}
}
