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
	if err := appendPublicationAudit(ctx, tx, value, "publication.pending", ""); err != nil {
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
	if err := appendPublicationAudit(ctx, tx, value, "publication.predecessor", ""); err != nil {
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
		if err := projectPromotionIssue(ctx, tx, finalization.Promotion, now); err != nil {
			return model.PublicationFinalization{}, err
		}
	} else if finalization.Run.ID != "" {
		if err := projectRunIssue(ctx, tx, finalization.Run, "", now); err != nil {
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
	if err := appendPublicationAudit(ctx, tx, finalization.Publication, "publication."+string(status), failure); err != nil {
		return model.PublicationFinalization{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.PublicationFinalization{}, fmt.Errorf("commit publication finalization: %w", err)
	}
	return finalization, nil
}

func appendPublicationAudit(ctx context.Context, tx *sql.Tx, publication model.Publication, action, failure string) error {
	details, err := publicationAuditDetails(publication, failure)
	if err != nil {
		return err
	}
	if _, err := appendAuditAction(ctx, tx, model.AuditActionSpec{
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
