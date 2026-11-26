package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/stateport"
)

type acceptedBinding struct {
	Resume                      *model.RunOrigin `json:",omitempty"`
	Request                     model.CIRequest
	RunID, Admission, Rejection string
	Release                     bool
	RerunOf                     string
	PromotionID                 string
	Child                       *nativebinding.ChildBinding
	Actions                     []model.AuditAction
}
type receiveBinding struct {
	Repository string
	Actor      actor.Actor
	Received   []model.CIRequest   `json:",omitempty"`
	Heads      map[string][]string `json:",omitempty"`
	Runs       []acceptedBinding
	Deletions  []stateport.BranchDeletion
}
type preparedBatch struct {
	stateport.BatchAcceptance

	body    []byte
	binding receiveBinding
	events  []stateport.EventWrite
}

func prepareBatch(input stateport.BatchAcceptance) (preparedBatch, error) {
	batch := preparedBatch{BatchAcceptance: input}
	if !validReceiveID(input.ReceiveID) || !validRepository(input.Repository) || input.Actor.Identity == "" || input.Actor.AuthMethod == "" {
		return batch, refuse(invalid, nil)
	}
	batch.Runs = append([]stateport.Acceptance(nil), input.Runs...)
	batch.Deletions = append([]stateport.BranchDeletion(nil), input.Deletions...)
	if err := prepareDeletions(&batch); err != nil {
		return batch, err
	}
	receipt := receiveBinding{Repository: input.Repository, Actor: input.Actor.Clone(), Deletions: batch.Deletions}
	var err error
	batch.Received, err = prepareReceived(input)
	if err != nil {
		return batch, err
	}
	receipt.Received = batch.Received
	batch.events = append([]stateport.EventWrite(nil), input.Events...)
	if err := prepareAdmissions(&batch, &receipt); err != nil {
		return batch, err
	}
	body, err := json.Marshal(receipt)
	batch.body = body
	batch.binding = receipt
	return batch, err
}

// AcceptBatch records the decided batch without consulting mutable authority or policy.
// A replay returns recorded outcomes with Fresh false and never creates new work.
func (s *Store) AcceptBatch(ctx context.Context, input stateport.BatchAcceptance) (stateport.AcceptanceResult, error) {
	batch, err := prepareBatch(input)
	if err != nil {
		return stateport.AcceptanceResult{}, err
	}
	var result stateport.AcceptanceResult
	err = s.observe(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = s.acceptBatch(ctx, tx, batch)
		return err
	}, batch.events...)
	if err != nil {
		return stateport.AcceptanceResult{}, err
	}
	return result, nil
}

func (s *Store) acceptBatch(ctx context.Context, tx *sql.Tx, batch preparedBatch) (stateport.AcceptanceResult, error) {
	var stored []byte
	err := tx.QueryRowContext(ctx, `SELECT binding FROM receives WHERE id=?`, batch.ReceiveID).Scan(&stored)
	if err == nil {
		request, err := receivedRequest(stored)
		if err != nil {
			return stateport.AcceptanceResult{}, err
		}
		if string(request) != string(batch.body) {
			return stateport.AcceptanceResult{}, refuse(receiveChanged, nil)
		}
		runs, err := loadReceivedRuns(ctx, tx, batch.ReceiveID)
		if err != nil {
			return stateport.AcceptanceResult{}, err
		}
		return stateport.AcceptanceResult{Runs: runs}, unchanged{}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return stateport.AcceptanceResult{}, databaseError(err)
	}
	runs, err := s.writeBatch(ctx, tx, batch)
	return stateport.AcceptanceResult{Runs: runs, Fresh: err == nil}, err
}

func (s *Store) writeBatch(ctx context.Context, tx *sql.Tx, batch preparedBatch) ([]stateport.Run, error) {
	repoID, err := repositoryNumber(ctx, tx, batch.Repository)
	if err != nil {
		return nil, err
	}
	body, err := captureReceivedHeads(ctx, tx, repoID, batch)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO receives VALUES(?,?,?)`, batch.ReceiveID, repoID, body); err != nil {
		return nil, databaseError(err)
	}
	if len(batch.Runs) == 0 && len(batch.Deletions) == 0 {
		if err := s.appendAudit(ctx, tx, mutation{batch.Actor.Identity, "receive.accept", "receive", batch.ReceiveID, nil}); err != nil {
			return nil, err
		}
	}
	for _, deletion := range batch.Deletions {
		if err := s.writeDeletion(ctx, tx, deletion); err != nil {
			return nil, err
		}
	}
	result := make([]stateport.Run, 0, len(batch.Runs))
	for position, item := range batch.Runs {
		run, err := s.acceptRun(ctx, tx, item)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO receive_runs VALUES(?,?,?)`, batch.ReceiveID, position, run.ID); err != nil {
			return nil, databaseError(err)
		}
		result = append(result, run)
	}
	return result, nil
}

func (s *Store) acceptRun(ctx context.Context, tx *sql.Tx, input stateport.Acceptance) (stateport.Run, error) {
	run, err := s.insertRun(ctx, tx, input)
	if err != nil {
		return stateport.Run{}, err
	}
	if err := s.linkPromotion(ctx, tx, input, run); err != nil {
		return stateport.Run{}, err
	}
	details := map[string]any{"admission_digest": run.Admission.Digest, "promotion_id": input.PromotionID}
	if run.HasAdmission {
		details["plan_digest"] = hashBytes([]byte(encode(run.Admission.Plan)))
	} else {
		details["rejection_digest"] = hashBytes([]byte(encode(input.Rejected)))
	}
	if err := s.appendAudit(ctx, tx, mutation{input.Request.Actor.Identity, "run.accept", "run", run.ID, details}); err != nil {
		return stateport.Run{}, err
	}
	if input.Resume != nil {
		if err := s.finishInPodPredecessor(ctx, tx, input, run); err != nil {
			return stateport.Run{}, err
		}
		if err := s.appendAudit(ctx, tx, mutation{input.Request.Actor.Identity, "run.resume", "run", run.ID, resumeAudit(input, run)}); err != nil {
			return stateport.Run{}, err
		}
	}
	for _, action := range input.Actions {
		m, err := actionMutation(action)
		if err != nil {
			return stateport.Run{}, err
		}
		if err := s.appendAudit(ctx, tx, m); err != nil {
			return stateport.Run{}, err
		}
	}
	return run.Clone(), nil
}

func validReceiveID(id string) bool { return len(id) > 0 && len(id) <= 512 }

// AcceptedRuns reads all outcomes of a receive, including rejected admissions.
func (s *Store) AcceptedRuns(ctx context.Context, id string) ([]stateport.Run, error) {
	tx, err := s.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, databaseError(err)
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM receives WHERE id=?`, id).Scan(&exists); err != nil {
		return nil, databaseError(err)
	}
	return loadReceivedRuns(ctx, tx, id)
}

func loadReceivedRuns(ctx context.Context, tx *sql.Tx, id string) ([]stateport.Run, error) {
	rows, err := tx.QueryContext(ctx, `SELECT run_id FROM receive_runs WHERE receive_id=? ORDER BY position`, id)
	if err != nil {
		return nil, databaseError(err)
	}
	var ids []string
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			_ = rows.Close()
			return nil, databaseError(err)
		}
		ids = append(ids, runID)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, databaseError(err)
	}
	result := make([]stateport.Run, 0, len(ids))
	for _, runID := range ids {
		run, err := loadRun(ctx, tx, runID)
		if err != nil {
			return nil, err
		}
		result = append(result, run)
	}
	return result, nil
}

func bindingFor(input stateport.Acceptance, view admissioncontract.PreparedView) acceptedBinding {
	binding := acceptedBinding{Resume: input.Resume, PromotionID: input.PromotionID, Request: input.Request, RunID: view.RunID, Admission: view.Digest, Release: input.Release,
		RerunOf: input.RerunOf, Child: input.Child, Actions: input.Actions}
	if input.Rejected != nil {
		binding.RunID = input.Rejected.ID
		binding.Rejection = hashBytes([]byte(encode(input.Rejected)))
	}
	return binding
}

func prepareDeletions(batch *preparedBatch) error {
	for i := range batch.Deletions {
		d := &batch.Deletions[i]
		if d.Request.ReceiveID == "" {
			d.Request.ReceiveID = batch.ReceiveID
		}
		if d.Request.ReceiveID != batch.ReceiveID || d.Request.Repo != batch.Repository || encode(d.Request.Actor) != encode(batch.Actor) {
			return refuse(invalid, nil)
		}
		if err := validateDeletion(*d); err != nil {
			return err
		}
	}
	return nil
}

func prepareAdmissions(batch *preparedBatch, receipt *receiveBinding) error {
	for i := range batch.Runs {
		item := &batch.Runs[i]
		if item.Request.ReceiveID == "" {
			item.Request.ReceiveID = batch.ReceiveID
		}
		if item.Request.ReceiveID != batch.ReceiveID || item.Request.Repo != batch.Repository || encode(item.Request.Actor) != encode(batch.Actor) {
			return refuse(invalid, nil)
		}
		binding, _, err := acceptance(*item)
		if err != nil {
			return err
		}
		for _, action := range item.Actions {
			if _, err := actionMutation(action); err != nil {
				return err
			}
		}
		receipt.Runs = append(receipt.Runs, binding)
		batch.events = append(batch.events, item.Events...)
	}
	return nil
}
