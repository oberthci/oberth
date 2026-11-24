package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

// ReceivedHead reads receive evidence, never mutable Git tracking state.
func (s *Store) ReceivedHead(ctx context.Context, q stateport.ReceivedRef) (*string, error) {
	if q.RunID == "" {
		return s.lastReceivedHead(ctx, q)
	}
	seen := map[string]bool{}
	for q.RunID != "" && !seen[q.RunID] {
		seen[q.RunID] = true
		var body []byte
		err := s.readers.QueryRowContext(ctx, `SELECT r.binding FROM receives r
JOIN receive_runs rr ON rr.receive_id=r.id WHERE rr.run_id=?`, q.RunID).Scan(&body)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, databaseError(err)
		}
		var binding receiveBinding
		if err := json.Unmarshal(body, &binding); err != nil {
			return nil, refuse(corrupt, err)
		}
		head, previous := bindingReceivedHead(q, binding)
		if head != nil {
			return head, nil
		}
		q.RunID = previous
	}
	return nil, nil
}

func (s *Store) lastReceivedHead(ctx context.Context, q stateport.ReceivedRef) (*string, error) {
	var previous string
	err := s.readers.QueryRowContext(ctx, `SELECT h.previous_sha FROM receive_heads h
JOIN repositories r ON r.id=h.repo_id WHERE r.canonical=? AND h.ref=? AND h.object_sha=?`, q.Repo, q.Ref, q.ObjectSHA).Scan(&previous)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, databaseError(err)
	}
	return &previous, nil
}

func bindingReceivedHead(q stateport.ReceivedRef, binding receiveBinding) (*string, string) {
	for _, received := range binding.Received {
		if received.Repo == q.Repo && received.Ref == q.Ref && received.ObjectSHA == q.ObjectSHA {
			return new(received.PreviousSHA), ""
		}
	}
	for _, run := range binding.Runs {
		if run.RunID == q.RunID {
			return nil, run.RerunOf
		}
	}
	return nil, ""
}

func prepareReceived(input stateport.BatchAcceptance) ([]model.CIRequest, error) {
	if len(input.Received) > stateport.MaxReceivedRefs {
		return nil, refuse(invalid, nil)
	}
	refs := map[string]bool{}
	received := make([]model.CIRequest, len(input.Received))
	for i, r := range input.Received {
		if refs[r.Ref] {
			return nil, refuse(invalid, nil)
		}
		refs[r.Ref] = true
		if err := validateReceived(input, r); err != nil {
			return nil, err
		}
		received[i] = r.Clone()
	}
	return received, nil
}

func validateReceived(input stateport.BatchAcceptance, r model.CIRequest) error {
	if r.ReceiveID != input.ReceiveID || r.Repo != input.Repository || encode(r.Actor) != encode(input.Actor) || !stateport.ValidReceivedRef(r) {
		return refuse(invalid, nil)
	}
	return nil
}

func writeReceivedHeads(ctx context.Context, tx *sql.Tx, repoID int64, received []model.CIRequest) error {
	for _, r := range received {
		if _, err := tx.ExecContext(ctx, `INSERT INTO receive_heads VALUES(?,?,?,?)
ON CONFLICT(repo_id,ref) DO UPDATE SET object_sha=excluded.object_sha,previous_sha=excluded.previous_sha`, repoID, r.Ref, r.ObjectSHA, r.PreviousSHA); err != nil {
			return databaseError(err)
		}
	}
	return nil
}
