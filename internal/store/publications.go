package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

const publicationColumns = `
sequence, id, repo_id, run_id, promotion_id, ref_kind, ref, previous_sha,
previous_known, result_sha, actor, status, error, created_at, updated_at`

// BeginPublication durably records the exact external Git mutation before it
// can happen. When a run owns the publication, changing its phase in the same
// transaction keeps restart recovery from mistaking it for an orphaned Job.
func (s *Store) BeginPublication(ctx context.Context, spec model.PublicationSpec) (model.Publication, error) {
	if spec.PreviousSHA != "" {
		spec.PreviousKnown = true
	}
	if err := validatePublicationSpec(spec); err != nil {
		return model.Publication{}, err
	}
	id, err := randomID()
	if err != nil {
		return model.Publication{}, fmt.Errorf("generate publication id: %w", err)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Publication{}, fmt.Errorf("begin publication: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var run model.Run
	if spec.RunID != "" {
		run, err = scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, spec.RunID))
		if errors.Is(err, sql.ErrNoRows) {
			return model.Publication{}, fmt.Errorf("%w: publication run", ErrNotFound)
		}
		if err != nil {
			return model.Publication{}, fmt.Errorf("load publication run: %w", err)
		}
		if run.Status != model.RunRunning || run.RepoID != spec.RepoID || run.Actor != spec.Actor ||
			!strings.EqualFold(run.SHA, spec.ResultSHA) {
			return model.Publication{}, fmt.Errorf("%w: publication does not match active run", ErrInvalidState)
		}
		if spec.PromotionID == "" && (run.RefKind != spec.RefKind || run.Ref != spec.Ref) {
			return model.Publication{}, fmt.Errorf("%w: publication ref does not match run", ErrInvalid)
		}
	}

	if spec.PromotionID != "" {
		promotion, promotionErr := scanPromotion(tx.QueryRowContext(ctx, `
SELECT sequence, id, repo_id, source_branch, source_sha, target_ref, previous_sha,
       result_sha, actor, status, run_id, error, created_at, updated_at
FROM promotions WHERE id = ?`, spec.PromotionID))
		if errors.Is(promotionErr, sql.ErrNoRows) {
			return model.Publication{}, fmt.Errorf("%w: publication promotion", ErrNotFound)
		}
		if promotionErr != nil {
			return model.Publication{}, fmt.Errorf("load publication promotion: %w", promotionErr)
		}
		if promotion.Status != model.PromotionPending || promotion.RepoID != spec.RepoID ||
			promotion.Actor != spec.Actor || promotion.TargetRef != spec.Ref || spec.RefKind != model.RefBranch ||
			!strings.EqualFold(promotion.PreviousSHA, spec.PreviousSHA) ||
			!strings.EqualFold(promotion.ResultSHA, spec.ResultSHA) || promotion.RunID != spec.RunID {
			return model.Publication{}, fmt.Errorf("%w: publication does not match pending promotion", ErrInvalidState)
		}
	}

	value, err := scanPublication(tx.QueryRowContext(ctx, `
INSERT INTO publications(
    id, repo_id, run_id, promotion_id, ref_kind, ref, previous_sha, previous_known,
    result_sha, actor, status, created_at, updated_at
)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)
RETURNING `+publicationColumns,
		id, spec.RepoID, nullableID(spec.RunID), nullableID(spec.PromotionID), spec.RefKind,
		spec.Ref, strings.ToLower(spec.PreviousSHA), spec.PreviousKnown,
		strings.ToLower(spec.ResultSHA), spec.Actor, now, now))
	if err != nil {
		return model.Publication{}, fmt.Errorf("insert publication intent: %w", err)
	}
	if spec.RunID != "" {
		result, updateErr := tx.ExecContext(ctx, `
UPDATE runs SET phase = 'publishing', updated_at = ?
WHERE id = ? AND status = 'running'`, now, spec.RunID)
		if updateErr != nil {
			return model.Publication{}, fmt.Errorf("mark run publishing: %w", updateErr)
		}
		if err := requireChanged("active publication run", result); err != nil {
			return model.Publication{}, err
		}
	}
	if err := s.appendPublicationAudit(ctx, tx, value, "publication.pending", ""); err != nil {
		return model.Publication{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Publication{}, fmt.Errorf("commit publication intent: %w", err)
	}
	return value, nil
}

func validatePublicationSpec(spec model.PublicationSpec) error {
	if spec.RepoID <= 0 || (strings.TrimSpace(spec.RunID) == "" && strings.TrimSpace(spec.PromotionID) == "") ||
		!spec.RefKind.Valid() || strings.TrimSpace(spec.Ref) == "" || !validOID(spec.ResultSHA) ||
		strings.TrimSpace(spec.Actor) == "" {
		return fmt.Errorf("%w: publication fields are invalid", ErrInvalid)
	}
	if spec.PreviousSHA != "" && !validOID(spec.PreviousSHA) {
		return fmt.Errorf("%w: previous publication SHA is invalid", ErrInvalid)
	}
	if !spec.PreviousKnown && spec.PreviousSHA != "" {
		return fmt.Errorf("%w: unknown predecessor cannot name a SHA", ErrInvalid)
	}
	return nil
}

// SetPublicationPredecessor durably binds an awaiting publication to the
// exact upstream state observed before any external mutation is attempted.
func (s *Store) SetPublicationPredecessor(ctx context.Context, id, sha string, exists bool) (model.Publication, error) {
	if strings.TrimSpace(id) == "" || (exists && !validOID(sha)) || (!exists && strings.TrimSpace(sha) != "") {
		return model.Publication{}, fmt.Errorf("%w: publication predecessor is invalid", ErrInvalid)
	}
	sha = strings.ToLower(sha)
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Publication{}, fmt.Errorf("begin publication predecessor: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanPublication(tx.QueryRowContext(ctx, `SELECT `+publicationColumns+` FROM publications WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Publication{}, fmt.Errorf("%w: publication", ErrNotFound)
	}
	if err != nil {
		return model.Publication{}, fmt.Errorf("load publication predecessor: %w", err)
	}
	if current.Status != model.PublicationPending {
		return model.Publication{}, fmt.Errorf("%w: publication is already terminal", ErrInvalidState)
	}
	if current.PreviousKnown {
		if strings.EqualFold(current.PreviousSHA, sha) {
			return current, nil
		}
		return model.Publication{}, fmt.Errorf("%w: publication predecessor is already bound", ErrInvalidState)
	}
	value, err := scanPublication(tx.QueryRowContext(ctx, `
UPDATE publications SET previous_sha = ?, previous_known = 1, updated_at = ?
WHERE id = ? AND status = 'pending' AND previous_known = 0
RETURNING `+publicationColumns, sha, now, id))
	if err != nil {
		return model.Publication{}, fmt.Errorf("bind publication predecessor: %w", err)
	}
	if err := s.appendPublicationAudit(ctx, tx, value, "publication.predecessor", ""); err != nil {
		return model.Publication{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Publication{}, fmt.Errorf("commit publication predecessor: %w", err)
	}
	return value, nil
}

func (s *Store) Publication(ctx context.Context, id string) (model.Publication, error) {
	value, err := scanPublication(s.db.QueryRowContext(ctx, `
SELECT `+publicationColumns+` FROM publications WHERE id = ?`, id))
	if err != nil {
		return model.Publication{}, translateNotFound("publication", err)
	}
	return value, nil
}

func (s *Store) PendingPublications(ctx context.Context) ([]model.Publication, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT `+publicationColumns+` FROM publications
WHERE status = 'pending' ORDER BY sequence`)
	if err != nil {
		return nil, fmt.Errorf("list pending publications: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var publications []model.Publication
	for rows.Next() {
		value, scanErr := scanPublication(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan pending publication: %w", scanErr)
		}
		publications = append(publications, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending publications: %w", err)
	}
	return publications, nil
}

// FinalizePublication closes the outbox obligation and every attached durable
// owner in one SQLite transaction. There is no state in which the external ref
// is recorded as delivered while its run or promotion remains pending.
func (s *Store) FinalizePublication(ctx context.Context, id string, status model.PublicationStatus, failure string) (model.PublicationFinalization, error) {
	if strings.TrimSpace(id) == "" || !status.Terminal() {
		return model.PublicationFinalization{}, fmt.Errorf("%w: terminal publication result is required", ErrInvalid)
	}
	if status == model.PublicationDelivered {
		failure = ""
	} else if strings.TrimSpace(failure) == "" {
		failure = "publication failed"
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.PublicationFinalization{}, fmt.Errorf("begin publication finalization: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	publication, err := scanPublication(tx.QueryRowContext(ctx, `
SELECT `+publicationColumns+` FROM publications WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.PublicationFinalization{}, fmt.Errorf("%w: publication", ErrNotFound)
	}
	if err != nil {
		return model.PublicationFinalization{}, fmt.Errorf("load publication for finalization: %w", err)
	}
	if publication.Status != model.PublicationPending {
		return model.PublicationFinalization{}, fmt.Errorf("%w: publication is already terminal", ErrInvalidState)
	}

	finalization := model.PublicationFinalization{}
	if publication.RunID != "" {
		runStatus, phase := model.RunPassed, "passed"
		if status == model.PublicationFailed {
			runStatus, phase = model.RunFailed, "publishing"
		}
		finalization.Run, err = scanRun(tx.QueryRowContext(ctx, `
UPDATE runs
SET status = ?, phase = ?, error = ?, finished_at = ?, updated_at = ?
WHERE id = ? AND status = 'running' AND phase = 'publishing'
RETURNING `+runColumns, runStatus, phase, failure, now, now, publication.RunID))
		if errors.Is(err, sql.ErrNoRows) {
			return model.PublicationFinalization{}, fmt.Errorf("%w: publication run is not active", ErrInvalidState)
		}
		if err != nil {
			return model.PublicationFinalization{}, fmt.Errorf("finalize publication run: %w", err)
		}
	}
	if publication.PromotionID != "" {
		promotionStatus := model.PromotionPassed
		if status == model.PublicationFailed {
			promotionStatus = model.PromotionFailed
		}
		finalization.Promotion, err = scanPromotion(tx.QueryRowContext(ctx, `
UPDATE promotions
SET status = ?, error = ?, updated_at = ?
WHERE id = ? AND status = 'pending'
RETURNING sequence, id, repo_id, source_branch, source_sha, target_ref, previous_sha,
          result_sha, actor, status, run_id, error, created_at, updated_at`,
			promotionStatus, failure, now, publication.PromotionID))
		if errors.Is(err, sql.ErrNoRows) {
			return model.PublicationFinalization{}, fmt.Errorf("%w: publication promotion is not pending", ErrInvalidState)
		}
		if err != nil {
			return model.PublicationFinalization{}, fmt.Errorf("finalize publication promotion: %w", err)
		}
	}
	if finalization.Promotion.ID != "" {
		if err := s.projectPromotionIssue(ctx, tx, finalization.Promotion, now); err != nil {
			return model.PublicationFinalization{}, err
		}
	} else if finalization.Run.ID != "" {
		if err := s.projectRunIssue(ctx, tx, finalization.Run, "", now); err != nil {
			return model.PublicationFinalization{}, err
		}
	}
	finalization.Publication, err = scanPublication(tx.QueryRowContext(ctx, `
UPDATE publications SET status = ?, error = ?, updated_at = ?
WHERE id = ? AND status = 'pending'
RETURNING `+publicationColumns, status, failure, now, publication.ID))
	if err != nil {
		return model.PublicationFinalization{}, fmt.Errorf("finalize publication: %w", err)
	}
	if err := s.appendPublicationAudit(ctx, tx, finalization.Publication, "publication."+string(status), failure); err != nil {
		return model.PublicationFinalization{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.PublicationFinalization{}, fmt.Errorf("commit publication finalization: %w", err)
	}
	return finalization, nil
}

func (s *Store) appendPublicationAudit(ctx context.Context, tx *sql.Tx, publication model.Publication, action, failure string) error {
	details, err := publicationAuditDetails(publication, failure)
	if err != nil {
		return err
	}
	if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{
		Actor: publication.Actor, Action: action, ResourceType: "publication",
		ResourceID: publication.ID, Details: details,
	}, safeAuditTime(publication)); err != nil {
		return fmt.Errorf("append publication audit: %w", err)
	}
	return nil
}

func publicationAuditDetails(publication model.Publication, failure string) (string, error) {
	details, err := json.Marshal(map[string]string{
		"repo_id":        fmt.Sprint(publication.RepoID),
		"ref_kind":       string(publication.RefKind),
		"ref":            publication.Ref,
		"previous_sha":   publication.PreviousSHA,
		"previous_known": fmt.Sprint(publication.PreviousKnown),
		"result_sha":     publication.ResultSHA,
		"run_id":         publication.RunID,
		"promotion_id":   publication.PromotionID,
		"error":          failure,
	})
	if err != nil {
		return "", fmt.Errorf("encode publication audit: %w", err)
	}
	return string(details), nil
}

type publicationAuditQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func verifyPendingPublicationIntents(ctx context.Context, querier publicationAuditQuerier) error {
	rows, err := querier.QueryContext(ctx, `
SELECT `+publicationColumns+` FROM publications
WHERE status = 'pending' ORDER BY sequence`)
	if err != nil {
		return fmt.Errorf("verify pending publication intents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var pending []model.Publication
	for rows.Next() {
		publication, scanErr := scanPublication(rows)
		if scanErr != nil {
			return fmt.Errorf("verify pending publication intent row: %w", scanErr)
		}
		pending = append(pending, publication)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("verify pending publication intents: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close pending publication intent rows: %w", err)
	}
	for _, publication := range pending {
		var actor, action, details string
		if err := querier.QueryRowContext(ctx, `
SELECT actor, action, details FROM audit_actions
WHERE resource_type = 'publication' AND resource_id = ?
ORDER BY id DESC LIMIT 1`, publication.ID).Scan(&actor, &action, &details); err != nil {
			return fmt.Errorf("pending publication %s has no chained audit intent: %w", publication.ID, err)
		}
		if actor != publication.Actor || (action != "publication.pending" && action != "publication.predecessor") {
			return fmt.Errorf("pending publication %s does not match its latest chained audit action", publication.ID)
		}
		expected, err := publicationAuditDetails(publication, "")
		if err != nil {
			return err
		}
		if details != expected {
			return fmt.Errorf("pending publication %s differs from its chained audit intent", publication.ID)
		}
	}
	return nil
}

// safeAuditTime keeps audit ordering aligned with the state transition without
// adding a second clock read inside a transaction.
func safeAuditTime(publication model.Publication) int64 {
	if !publication.UpdatedAt.IsZero() {
		return publication.UpdatedAt.UnixNano()
	}
	return publication.CreatedAt.UnixNano()
}

// RetryFailedPublication atomically transitions a publishing-failed run (or
// promotion) back to running/publishing and resets the failed publication to
// pending so the delivery path can re-attempt the upstream push. The actor is
// the uplink identity requesting the retry.
//
// Preconditions:
//   - A failed publication for the given run/promotion exists.
//   - The owning run is status='failed', phase='publishing' (or the owning
//     promotion is status='failed').
//
// The publication's previous_sha is cleared (previous_known = 0) so the
// delivery path re-reads the exact remote state before deciding whether a
// mutation is needed. Idempotent upstream state is handled by the existing
// classifyRemotePublication logic.
func (s *Store) RetryFailedPublication(ctx context.Context, runID, promotionID, actor string) (model.Publication, error) {
	if strings.TrimSpace(runID) == "" && strings.TrimSpace(promotionID) == "" {
		return model.Publication{}, fmt.Errorf("%w: retry requires a run or promotion ID", ErrInvalid)
	}
	if strings.TrimSpace(actor) == "" {
		return model.Publication{}, fmt.Errorf("%w: retry actor is required", ErrInvalid)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Publication{}, fmt.Errorf("begin publication retry: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Find the failed publication for this run or promotion.
	var query string
	var queryArg string
	if runID != "" {
		query = `SELECT ` + publicationColumns + ` FROM publications WHERE run_id = ? AND status = 'failed' ORDER BY sequence DESC LIMIT 1`
		queryArg = runID
	} else {
		query = `SELECT ` + publicationColumns + ` FROM publications WHERE promotion_id = ? AND status = 'failed' ORDER BY sequence DESC LIMIT 1`
		queryArg = promotionID
	}
	failed, err := scanPublication(tx.QueryRowContext(ctx, query, queryArg))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Publication{}, fmt.Errorf("%w: no failed publication for this run or promotion", ErrInvalidState)
	}
	if err != nil {
		return model.Publication{}, fmt.Errorf("load failed publication: %w", err)
	}

	// Verify and transition the owning run/promotion back to the publishing state.
	if failed.RunID != "" {
		result, updateErr := tx.ExecContext(ctx, `
UPDATE runs SET status = 'running', phase = 'publishing', error = '', finished_at = NULL, updated_at = ?
WHERE id = ? AND status = 'failed' AND phase = 'publishing'`, now, failed.RunID)
		if updateErr != nil {
			return model.Publication{}, fmt.Errorf("reactivate publishing-failed run: %w", updateErr)
		}
		if err := requireChanged("publishing-failed run", result); err != nil {
			return model.Publication{}, fmt.Errorf("%w: run is not in a retryable publishing-failed state", ErrInvalidState)
		}
	}
	if failed.PromotionID != "" {
		result, updateErr := tx.ExecContext(ctx, `
UPDATE promotions SET status = 'pending', error = '', updated_at = ?
WHERE id = ? AND status = 'failed'`, now, failed.PromotionID)
		if updateErr != nil {
			return model.Publication{}, fmt.Errorf("reactivate publishing-failed promotion: %w", updateErr)
		}
		if err := requireChanged("publishing-failed promotion", result); err != nil {
			return model.Publication{}, fmt.Errorf("%w: promotion is not in a retryable failed state", ErrInvalidState)
		}
	}

	// Verify the audit mutation gate inside this transaction before mutating
	// any protected state. A gate failure leaves the run in failed/publishing
	// (retryable) without any state change.
	if err := verifyPendingPublicationIntents(ctx, tx); err != nil {
		return model.Publication{}, fmt.Errorf("%w: audit gate failed before retry: %w", ErrInvalidState, err)
	}

	// Reset the failed publication to pending. Clear previous_sha so the
	// delivery path re-reads the exact remote state before mutation.
	value, err := scanPublication(tx.QueryRowContext(ctx, `
UPDATE publications SET status = 'pending', error = '', previous_sha = '', previous_known = 0,
    actor = ?, updated_at = ?
WHERE id = ? AND status = 'failed'
RETURNING `+publicationColumns, actor, now, failed.ID))
	if err != nil {
		return model.Publication{}, fmt.Errorf("reset publication to pending: %w", err)
	}

	// Chain the informational retry action first, then chain the canonical
	// publication.pending action as the LATEST entry. The mutation gate
	// (verifyPendingPublicationSnapshot) requires every pending publication's
	// latest audit action to be publication.pending or publication.predecessor
	// with canonical details. Leaving publication.retry as the latest action
	// permanently breaks the gate.
	if err := s.appendPublicationAudit(ctx, tx, value, "publication.retry", "retrying failed publication "+failed.ID); err != nil {
		return model.Publication{}, err
	}
	if err := s.appendPublicationAudit(ctx, tx, value, "publication.pending", ""); err != nil {
		return model.Publication{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Publication{}, fmt.Errorf("commit publication retry: %w", err)
	}
	return value, nil
}

// ReconcileInconsistentPublications heals pending publications whose latest
// chained audit action is not one of the gate-valid actions (publication.pending
// or publication.predecessor). This state was produced by RetryFailedPublication
// in v0.16.18, which chained publication.retry as the latest action without a
// subsequent canonical publication.pending. The mutation gate then rejects every
// operation (including readiness), taking the server fully offline.
//
// For each inconsistent pending publication, this method chains a canonical
// publication.pending action through the normal append path so the hash chain
// stays gap-free. When the actor from the publication row is empty (impossible
// for a valid publication, but defended), the row is transitioned to failed
// instead.
//
// This method is idempotent: a publication whose latest action is already
// gate-valid is left untouched. It MUST run before the first gate evaluation
// and before readiness can report ready.
func (s *Store) ReconcileInconsistentPublications(ctx context.Context) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin publication reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
SELECT `+publicationColumns+` FROM publications
WHERE status = 'pending' ORDER BY sequence`)
	if err != nil {
		return 0, fmt.Errorf("list pending publications for reconciliation: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var pending []model.Publication
	for rows.Next() {
		publication, scanErr := scanPublication(rows)
		if scanErr != nil {
			return 0, fmt.Errorf("scan pending publication for reconciliation: %w", scanErr)
		}
		pending = append(pending, publication)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("list pending publications for reconciliation: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close pending publications for reconciliation: %w", err)
	}

	now := unixNano(s.now())
	healed := 0
	for _, publication := range pending {
		var actor, action, details string
		scanErr := tx.QueryRowContext(ctx, `
SELECT actor, action, details FROM audit_actions
WHERE resource_type = 'publication' AND resource_id = ?
ORDER BY id DESC LIMIT 1`, publication.ID).Scan(&actor, &action, &details)
		if scanErr != nil {
			return 0, fmt.Errorf("read latest audit action for publication %s: %w", publication.ID, scanErr)
		}

		// Check if the publication is already gate-valid.
		if action == "publication.pending" || action == "publication.predecessor" {
			expected, detailErr := publicationAuditDetails(publication, "")
			if detailErr != nil {
				return 0, detailErr
			}
			if actor == publication.Actor && details == expected {
				continue // already consistent
			}
		}

		// Inconsistent: chain a canonical publication.pending action to heal it.
		if strings.TrimSpace(publication.Actor) == "" {
			// Defensive: a publication with no actor cannot be healed to a
			// gate-valid state. Transition it to failed instead.
			if _, execErr := tx.ExecContext(ctx, `
UPDATE publications SET status = 'failed', error = 'reconciled: no actor for gate-consistent retry', updated_at = ?
WHERE id = ? AND status = 'pending'`, now, publication.ID); execErr != nil {
				return 0, fmt.Errorf("fail actorless publication %s: %w", publication.ID, execErr)
			}
			// Also put the owning run back to failed/publishing if it was reactivated.
			if publication.RunID != "" {
				if _, execErr := tx.ExecContext(ctx, `
UPDATE runs SET status = 'failed', phase = 'publishing',
    error = 'reconciled: publication had no actor', finished_at = ?, updated_at = ?
WHERE id = ? AND status = 'running' AND phase = 'publishing'`, now, now, publication.RunID); execErr != nil {
					return 0, fmt.Errorf("fail run for actorless publication %s: %w", publication.ID, execErr)
				}
			}
			// Re-read the now-failed publication for the audit action.
			failedPub, failErr := scanPublication(tx.QueryRowContext(ctx, `
SELECT `+publicationColumns+` FROM publications WHERE id = ?`, publication.ID))
			if failErr != nil {
				return 0, fmt.Errorf("re-read failed publication %s: %w", publication.ID, failErr)
			}
			if err := s.appendPublicationAudit(ctx, tx, failedPub, "publication.failed", "reconciled: no actor for gate-consistent retry"); err != nil {
				return 0, err
			}
			healed++
			continue
		}

		if err := s.appendPublicationAudit(ctx, tx, publication, "publication.pending", ""); err != nil {
			return 0, fmt.Errorf("heal publication %s: %w", publication.ID, err)
		}
		healed++
	}

	if healed > 0 {
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("commit publication reconciliation: %w", err)
		}
	}
	return healed, nil
}

func nullableID(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func scanPublication(row rowScanner) (model.Publication, error) {
	var value model.Publication
	var runID, promotionID sql.NullString
	var created, updated int64
	if err := row.Scan(&value.Sequence, &value.ID, &value.RepoID, &runID, &promotionID,
		&value.RefKind, &value.Ref, &value.PreviousSHA, &value.PreviousKnown, &value.ResultSHA, &value.Actor,
		&value.Status, &value.Error, &created, &updated); err != nil {
		return model.Publication{}, err
	}
	value.RunID, value.PromotionID = runID.String, promotionID.String
	value.CreatedAt, value.UpdatedAt = fromUnixNano(created), fromUnixNano(updated)
	return value, nil
}
