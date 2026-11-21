package store

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func acceptForTest(ctx context.Context, s *Store, input stateport.Acceptance) (stateport.Run, error) {
	recorded, err := s.AcceptBatch(ctx, stateport.BatchAcceptance{ReceiveID: input.Request.ReceiveID, Repository: input.Request.Repo, Actor: input.Request.Actor, Runs: []stateport.Acceptance{input}})
	rows := recorded.Runs
	if err != nil {
		return stateport.Run{}, err
	}
	return rows[0], nil
}

func eventForTest(ctx context.Context, s *Store, input stateport.EventWrite) (int64, error) {
	err := s.RecordAction(ctx, model.AuditAction{Actor: "operator", Action: "fixture.event", ResourceType: "event", ResourceID: input.Dedupe}, input)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.readers.QueryRowContext(ctx, `SELECT id FROM events WHERE dedupe=?`, input.Dedupe).Scan(&id)
	return id, databaseError(err)
}

func backupForTest(t *testing.T, s *Store) *Backup {
	t.Helper()
	backup, err := NewBackup(s.path)
	requireOK(t, err)
	return backup
}

func deletionForTest(request model.CIRequest) stateport.BranchDeletion {
	return stateport.BranchDeletion{Request: request, Publication: model.Publication{ID: "deletion-" + request.ReceiveID, Repo: request.Repo, Ref: request.Ref, PreviousSHA: new(request.PreviousSHA), Actor: request.Actor.Identity}}
}

func recordDeletion(t *testing.T, s *Store, d stateport.BranchDeletion, events ...stateport.EventWrite) error {
	t.Helper()
	_, err := s.AcceptBatch(t.Context(), stateport.BatchAcceptance{ReceiveID: d.Request.ReceiveID, Repository: d.Request.Repo, Actor: d.Request.Actor, Deletions: []stateport.BranchDeletion{d}, Events: events})
	return err
}
