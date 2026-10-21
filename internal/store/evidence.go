package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

var ErrEvidenceProtected = errors.New("run evidence is protected")

// PurgeRunEvidence commits protection checks and an audited intent atomically.
// Removal runs without a database transaction: the committed pending marker
// fences evidence readers until the outcome commits. Missing run rows are
// permitted so retention can reclaim orphaned files through the same audit path.
// A crash leaves the intent visible; retrying the same purge is safe.
func (s *Store) PurgeRunEvidence(ctx context.Context, actor, id, what, reason string, keepNewest int, remove func() (int64, error)) (model.EvidencePurge, error) {
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(reason) == "" || len(reason) > 512 || ValidateCancelReason(reason) != nil || (what != "logs" && what != "artifacts" && what != "both") || remove == nil || keepNewest < 0 {
		return model.EvidencePurge{}, fmt.Errorf("%w: actor, printable reason (1..512 bytes), and what=logs|artifacts|both are required", ErrInvalid)
	}
	purgeID, err := randomID()
	if err != nil {
		return model.EvidencePurge{}, err
	}
	value := model.EvidencePurge{ID: purgeID, RunID: id, What: what, Actor: actor, At: s.now().UTC(), Reason: reason, State: "pending"}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return value, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.checkEvidenceProtection(ctx, tx, id, keepNewest); err != nil {
		if errors.Is(err, ErrEvidenceProtected) {
			// A crash may have left earlier intents unresolved. A protected retry
			// cannot remove their evidence, but must close those same intent IDs.
			pending, readErr := runEvidencePurges(ctx, tx, id)
			if readErr != nil {
				return value, errors.Join(err, readErr)
			}
			for _, previous := range pending {
				if previous.State != "pending" {
					continue
				}
				previous.State, previous.Error = "refused", err.Error()
				previous.Actor, previous.At, previous.Reason = actor, value.At, reason
				if auditErr := s.auditEvidencePurge(ctx, tx, "run.purge", previous); auditErr != nil {
					return value, errors.Join(err, auditErr)
				}
			}
			value.State, value.Error = "refused", err.Error()
			return value, errors.Join(err, tx.Commit())
		}
		return value, err
	}
	if err := s.auditEvidencePurge(ctx, tx, "run.purge.pending", value); err != nil {
		return value, err
	}
	if err := tx.Commit(); err != nil {
		return value, err
	}

	var removalErr error
	value.Bytes, removalErr = remove()
	value.State = "complete"
	if removalErr != nil {
		value.State = "failed"
		value.Error = removalErr.Error()
	}
	resultTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return value, errors.Join(removalErr, err)
	}
	defer func() { _ = resultTx.Rollback() }()
	if err := s.auditEvidencePurge(ctx, resultTx, "run.purge", value); err != nil {
		return value, errors.Join(removalErr, err)
	}
	if err := resultTx.Commit(); err != nil {
		return value, errors.Join(removalErr, err)
	}
	return value, removalErr
}

func (s *Store) auditEvidencePurge(ctx context.Context, tx *sql.Tx, action string, value model.EvidencePurge) error {
	details, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: value.Actor, Action: action, ResourceType: "run", ResourceID: value.RunID, Details: string(details)}, unixNano(s.now()))
	return err
}

// Shared by the read-only sweep snapshot and the transactional purge check.
const evidenceProtectionSQL = `
 r.status IN ('queued','waiting','running') OR r.release = 1 OR r.ref_kind = 'tag'
 OR r.trigger IN ('tag','release','plan','apply')
 OR EXISTS(SELECT 1 FROM run_cancellations c WHERE c.run_id=r.id AND c.completed_at IS NULL)
 OR EXISTS(SELECT 1 FROM promotions p WHERE p.run_id=r.id
   OR (p.repo_id=r.repo_id AND p.status='pending' AND (p.source_sha=r.sha OR p.source_sha=r.tested_sha OR p.result_sha=r.tested_sha)))
 OR EXISTS(SELECT 1 FROM publications p WHERE p.run_id=r.id AND p.status='pending')
 OR EXISTS(SELECT 1 FROM trusted_plans p WHERE p.green_run_id=r.id OR p.plan_run_id=r.id)
 OR EXISTS(SELECT 1 FROM trusted_applies a WHERE a.run_id=r.id)
 OR EXISTS(SELECT 1 FROM issues i WHERE i.repo_id=r.repo_id AND i.branch=r.ref AND i.state='open')
 OR EXISTS(SELECT 1 FROM promotion_grants g WHERE g.repo_id=r.repo_id AND (g.source_sha=r.sha OR g.source_sha=r.tested_sha)
   AND g.revoked_at IS NULL AND g.consumed_at IS NULL AND g.expires_at>?)
 OR (? > 0 AND (SELECT count(*) FROM runs newer WHERE newer.repo_id=r.repo_id AND newer.ref_kind=r.ref_kind AND newer.ref=r.ref
    AND newer.queue_sequence>r.queue_sequence AND newer.status NOT IN ('queued','waiting','running')) < ?)`

func (s *Store) checkEvidenceProtection(ctx context.Context, tx *sql.Tx, id string, keepNewest int) error {
	var protected bool
	err := tx.QueryRowContext(ctx, `SELECT `+evidenceProtectionSQL+` FROM runs r WHERE r.id=?`, unixNano(s.now()), keepNewest, keepNewest, id).Scan(&protected)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // Orphaned evidence has no run to protect.
	}
	if err != nil {
		return translateNotFound("run evidence", err)
	}
	if protected {
		return fmt.Errorf("%w: run %s is active, retained for evidence, or among the newest runs", ErrEvidenceProtected, id)
	}
	return nil
}

type EvidenceRetentionRun struct {
	ID            string
	FinishedAt    *time.Time
	QueueSequence int64
	Protected     bool
}

// EvidenceRetentionRuns pre-filters a sweep with one read query. Purges still
// check protection in their intent transaction to cover changes since this read.
func (s *Store) EvidenceRetentionRuns(ctx context.Context, keepNewest int) (map[string]EvidenceRetentionRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.id, r.finished_at, r.queue_sequence, `+evidenceProtectionSQL+` FROM runs r`, unixNano(s.now()), keepNewest, keepNewest)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	values := make(map[string]EvidenceRetentionRun)
	for rows.Next() {
		var value EvidenceRetentionRun
		var finished sql.NullInt64
		if err := rows.Scan(&value.ID, &finished, &value.QueueSequence, &value.Protected); err != nil {
			return nil, err
		}
		value.FinishedAt = nullableTime(finished)
		values[value.ID] = value
	}
	return values, rows.Err()
}

// RunEvidencePurges projects the append-only audit intents and outcomes, so no
// run-row mutation or numbered schema migration is needed for purge markers.
func (s *Store) RunEvidencePurges(ctx context.Context, id string) ([]model.EvidencePurge, error) {
	return runEvidencePurges(ctx, s.db, id)
}

func runEvidencePurges(ctx context.Context, query interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, id string) ([]model.EvidencePurge, error) {
	rows, err := query.QueryContext(ctx, `SELECT details FROM audit_actions WHERE resource_type='run' AND resource_id=? AND action IN ('run.purge.pending','run.purge') ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var values []model.EvidencePurge
	positions := make(map[string]int)
	for rows.Next() {
		var details string
		if err := rows.Scan(&details); err != nil {
			return nil, err
		}
		var value model.EvidencePurge
		if err := json.Unmarshal([]byte(details), &value); err != nil {
			return nil, fmt.Errorf("read purge marker: %w", err)
		}
		// A result replaces only its matching intent, preserving earlier purges of
		// the other evidence class and earlier completed purges on a refused retry.
		if index, ok := positions[value.ID]; ok {
			values[index] = value
		} else {
			positions[value.ID] = len(values)
			values = append(values, value)
		}
	}
	return values, rows.Err()
}
