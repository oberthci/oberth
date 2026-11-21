package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

type mutation struct {
	actor, action, resource, id string
	details                     any
}

func (s *Store) change(ctx context.Context, m mutation, apply func(*sql.Tx) error, events ...stateport.EventWrite) error {
	if m.actor == "" {
		return refuse(invalid, nil)
	}
	if err := validateEvents(events); err != nil {
		return err
	}
	tx, done, err := s.beginWrite(ctx)
	if err != nil {
		return databaseError(err)
	}
	defer done()
	if err := apply(tx); err != nil {
		var same unchanged
		if errors.As(err, &same) {
			return nil
		}
		return err
	}
	if err := s.appendAudit(ctx, tx, m); err != nil {
		return err
	}
	if err := writeEvents(ctx, tx, events); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return commitFailure{databaseError(err)}
	}
	s.signalChange()
	return nil
}

func genesis() string { return stateport.AuditGenesis() }

func (s *Store) appendAudit(ctx context.Context, tx *sql.Tx, m mutation) error {
	var head stateport.AuditHead
	if err := tx.QueryRowContext(ctx, `SELECT id,hash FROM audit_head WHERE singleton=1`).Scan(&head.ID, &head.SHA256); err != nil {
		return databaseError(err)
	}
	details, err := json.Marshal(m.details)
	if err != nil {
		return refuse(invalid, err)
	}
	action := model.AuditAction{
		ID:           head.ID + 1,
		Actor:        m.actor,
		Action:       m.action,
		ResourceType: m.resource,
		ResourceID:   m.id,
		Details:      string(details),
		CreatedAt:    s.options.Clock().UTC(),
	}
	hash := stateport.AuditDigest(s.options.Protocol.AuditDomain, head.SHA256, action)
	_, err = tx.ExecContext(ctx, `INSERT INTO audit VALUES(?,?,?,?,?,?,?,?,?)`, action.ID, action.Actor, action.Action,
		action.ResourceType, action.ResourceID, action.Details, action.CreatedAt.Format(timestampFormat), head.SHA256, hash)
	if err != nil {
		return databaseError(err)
	}
	_, err = tx.ExecContext(ctx, `UPDATE audit_head SET id=?,hash=? WHERE singleton=1`, action.ID, hash)
	return databaseError(err)
}

// Audit verifies the returned page and its link to the preceding row.
func (s *Store) Audit(ctx context.Context, after int64, limit int) ([]model.AuditAction, stateport.AuditHead, error) {
	if after < 0 || limit < 1 || limit > 200 {
		return nil, stateport.AuditHead{}, refuse(invalid, nil)
	}
	tx, err := s.auditReaders.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, stateport.AuditHead{}, databaseError(err)
	}
	defer func() { _ = tx.Rollback() }()
	head, err := auditHead(ctx, tx)
	if err != nil {
		return nil, head, err
	}
	actions, last, err := s.auditPage(ctx, tx, after, limit)
	if err != nil {
		return nil, head, err
	}
	if (len(actions) < limit || last.ID >= head.ID) && last != head {
		return nil, head, refuse(corrupt, nil)
	}
	return actions, head, nil
}

func auditHead(ctx context.Context, tx *sql.Tx) (stateport.AuditHead, error) {
	var stored stateport.AuditHead
	if err := tx.QueryRowContext(ctx, `SELECT id,hash FROM audit_head WHERE singleton=1`).Scan(&stored.ID, &stored.SHA256); err != nil {
		return stored, databaseError(err)
	}
	tail := stateport.AuditHead{SHA256: genesis()}
	err := tx.QueryRowContext(ctx, `SELECT id,hash FROM audit ORDER BY id DESC LIMIT 1`).Scan(&tail.ID, &tail.SHA256)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return stored, databaseError(err)
	}
	if stored != tail {
		return stored, refuse(corrupt, nil)
	}
	return stored, nil
}

func (s *Store) auditPage(ctx context.Context, tx *sql.Tx, after int64, limit int) ([]model.AuditAction, stateport.AuditHead, error) {
	previous := stateport.AuditHead{SHA256: genesis()}
	err := tx.QueryRowContext(ctx, `SELECT id,hash FROM audit WHERE id<=? ORDER BY id DESC LIMIT 1`, after).Scan(&previous.ID, &previous.SHA256)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, previous, databaseError(err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,actor,action,resource_type,resource_id,details,created_at,previous,hash FROM audit WHERE id>? ORDER
BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, previous, databaseError(err)
	}
	defer func() { _ = rows.Close() }()
	actions := make([]model.AuditAction, 0)
	for rows.Next() {
		action, link, hash, err := scanAudit(rows)
		if err != nil {
			return nil, previous, err
		}
		if link != previous.SHA256 || hash != stateport.AuditDigest(s.options.Protocol.AuditDomain, link, action) {
			return nil, previous, refuse(corrupt, nil)
		}
		previous = stateport.AuditHead{ID: action.ID, SHA256: hash}
		actions = append(actions, action)
	}
	return actions, previous, databaseError(rows.Err())
}

func scanAudit(rows *sql.Rows) (model.AuditAction, string, string, error) {
	var action model.AuditAction
	var timestamp, previous, hash string
	err := rows.Scan(&action.ID, &action.Actor, &action.Action, &action.ResourceType, &action.ResourceID, &action.Details, &timestamp, &previous, &hash)
	if err != nil {
		return action, previous, hash, databaseError(err)
	}
	action.CreatedAt, err = time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return action, previous, hash, refuse(corrupt, err)
	}
	return action, previous, hash, nil
}

func verifyAudit(ctx context.Context, tx *sql.Tx, domain string) (stateport.AuditHead, error) {
	head := stateport.AuditHead{SHA256: genesis()}
	rows, err := tx.QueryContext(ctx, `SELECT id,actor,action,resource_type,resource_id,details,created_at,previous,hash FROM audit ORDER BY id`)
	if err != nil {
		return head, refuse(corrupt, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		action, previous, hash, err := scanAudit(rows)
		if err != nil {
			return head, refuse(corrupt, err)
		}
		if previous != head.SHA256 || hash != stateport.AuditDigest(domain, previous, action) {
			return head, refuse(corrupt, nil)
		}
		head = stateport.AuditHead{ID: action.ID, SHA256: hash}
	}
	if err := rows.Err(); err != nil {
		return head, refuse(corrupt, err)
	}
	var stored stateport.AuditHead
	err = tx.QueryRowContext(ctx, `SELECT id,hash FROM audit_head WHERE singleton=1`).Scan(&stored.ID, &stored.SHA256)
	if err != nil || head != stored {
		return head, refuse(corrupt, err)
	}
	return head, nil
}

type unchanged struct{}

func (unchanged) Error() string { return "unchanged" }

// commitFailure distinguishes an unconfirmed commit from a known rollback.
type commitFailure struct{ error }

func (e commitFailure) Unwrap() error { return e.error }
