package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/gitoid"
	"github.com/oberthci/oberth/internal/model"
)

const receiveEventColumns = `
id, actor, repo_id, ref_kind, ref, old_sha, object_sha, commit_sha,
outcome, run_id, created_at`

// RecordReceiveEvent durably acknowledges a deletion or rejected release. The
// event ID and its actor-bound audit record commit together, making replay a
// no-op after the first successful callback.
func (s *Store) RecordReceiveEvent(ctx context.Context, spec model.ReceiveEventSpec) (bool, error) {
	if err := validateReceiveEventSpec(spec, false); err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin receive event: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if duplicate, err := receiveDuplicate(ctx, tx, spec, false); duplicate || err != nil {
		return duplicate, err
	}
	now := unixNano(s.now())
	if err := insertReceiveEvent(ctx, tx, spec, "", now); err != nil {
		return false, err
	}
	if err := appendReceiveAudit(ctx, tx, spec, "", now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit receive event: %w", err)
	}
	return false, nil
}

// EnqueueReceiveEvent atomically deduplicates one receive callback, enqueues
// its exact run, records supersession obligations, and attributes the push.
func (s *Store) EnqueueReceiveEvent(ctx context.Context, event model.ReceiveEventSpec, runSpec model.RunSpec) (model.EnqueueRunResult, error) {
	if err := validateReceiveEventSpec(event, true); err != nil {
		return model.EnqueueRunResult{}, err
	}
	validatedRun, err := validateRunSpec(runSpec)
	if err != nil {
		return model.EnqueueRunResult{}, err
	}
	if err := matchReceiveRun(event, validatedRun); err != nil {
		return model.EnqueueRunResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("begin receive enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if duplicate, duplicateErr := receiveDuplicate(ctx, tx, event, true); duplicate || duplicateErr != nil {
		if duplicateErr != nil {
			return model.EnqueueRunResult{}, duplicateErr
		}
		var runID string
		if err := tx.QueryRowContext(ctx, `SELECT run_id FROM receive_events WHERE id = ?`, event.ID).Scan(&runID); err != nil {
			return model.EnqueueRunResult{}, fmt.Errorf("read duplicate receive run: %w", err)
		}
		run, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, runID))
		if err != nil {
			return model.EnqueueRunResult{}, fmt.Errorf("read duplicate receive run: %w", err)
		}
		return model.EnqueueRunResult{Run: run, Duplicate: true}, nil
	}
	runID, err := randomID()
	if err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("generate receive run id: %w", err)
	}
	now := unixNano(s.now())
	enqueued, err := s.enqueueRunTx(ctx, tx, validatedRun, runID, now)
	if err != nil {
		return model.EnqueueRunResult{}, err
	}
	if err := insertReceiveEvent(ctx, tx, event, enqueued.ID, now); err != nil {
		return model.EnqueueRunResult{}, err
	}
	if err := appendReceiveAudit(ctx, tx, event, enqueued.ID, now); err != nil {
		return model.EnqueueRunResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("commit receive enqueue: %w", err)
	}
	return enqueued, nil
}

func (s *Store) ReceiveEvent(ctx context.Context, id string) (model.ReceiveEvent, error) {
	value, err := scanReceiveEvent(s.db.QueryRowContext(ctx, `SELECT `+receiveEventColumns+` FROM receive_events WHERE id = ?`, id))
	if err != nil {
		return model.ReceiveEvent{}, translateNotFound("receive event", err)
	}
	return value, nil
}

func validateReceiveEventSpec(spec model.ReceiveEventSpec, queued bool) error {
	if strings.TrimSpace(spec.ID) == "" || len(spec.ID) > 200 || strings.TrimSpace(spec.Actor) == "" ||
		spec.RepoID <= 0 || !spec.RefKind.Valid() || strings.TrimSpace(spec.Ref) == "" {
		return fmt.Errorf("%w: receive event fields are invalid", ErrInvalid)
	}
	if spec.OldSHA != "" && !validOID(spec.OldSHA) {
		return fmt.Errorf("%w: receive old object ID is invalid", ErrInvalid)
	}
	if queued {
		if spec.Outcome != "queued" || !validOID(spec.ObjectSHA) || !validOID(spec.CommitSHA) {
			return fmt.Errorf("%w: queued receive object IDs are invalid", ErrInvalid)
		}
	} else if spec.Outcome != "deleted" && spec.Outcome != "release_rejected" && spec.Outcome != "release_delivered" {
		return fmt.Errorf("%w: receive outcome is invalid", ErrInvalid)
	}
	if (spec.Outcome == "release_rejected" || spec.Outcome == "release_delivered") &&
		(!validOID(spec.ObjectSHA) || !validOID(spec.CommitSHA)) {
		return fmt.Errorf("%w: terminal release object IDs are invalid", ErrInvalid)
	}
	return nil
}

func matchReceiveRun(event model.ReceiveEventSpec, run model.RunSpec) error {
	if event.Actor != run.Actor || event.RepoID != run.RepoID || event.RefKind != run.RefKind || event.Ref != run.Ref {
		return fmt.Errorf("%w: receive event and run identity differ", ErrInvalid)
	}
	if event.RefKind == model.RefTag {
		if !run.Release || !sameStoredOID(event.ObjectSHA, run.SHA) || !sameStoredOID(event.CommitSHA, run.TestedSHA) {
			return fmt.Errorf("%w: release run does not preserve tag object and commit", ErrInvalid)
		}
		return nil
	}
	if run.Release || !sameStoredOID(event.CommitSHA, run.SHA) || !sameStoredOID(event.CommitSHA, run.TestedSHA) {
		return fmt.Errorf("%w: branch run is not the accepted commit", ErrInvalid)
	}
	return nil
}

func receiveDuplicate(ctx context.Context, tx *sql.Tx, spec model.ReceiveEventSpec, requireRun bool) (bool, error) {
	value, err := scanReceiveEvent(tx.QueryRowContext(ctx, `SELECT `+receiveEventColumns+` FROM receive_events WHERE id = ?`, spec.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look up receive event: %w", err)
	}
	if value.Actor != spec.Actor || value.RepoID != spec.RepoID || value.RefKind != spec.RefKind || value.Ref != spec.Ref ||
		!sameStoredOID(value.OldSHA, spec.OldSHA) || !sameStoredOID(value.ObjectSHA, spec.ObjectSHA) ||
		!sameStoredOID(value.CommitSHA, spec.CommitSHA) || value.Outcome != spec.Outcome || (requireRun && value.RunID == "") {
		return false, fmt.Errorf("%w: receive event ID was reused with different contents", ErrInvalidState)
	}
	return true, nil
}

func insertReceiveEvent(ctx context.Context, tx *sql.Tx, spec model.ReceiveEventSpec, runID string, now int64) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO receive_events(id, actor, repo_id, ref_kind, ref, old_sha, object_sha, commit_sha, outcome, run_id, created_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		spec.ID, spec.Actor, spec.RepoID, spec.RefKind, spec.Ref, strings.ToLower(spec.OldSHA),
		strings.ToLower(spec.ObjectSHA), strings.ToLower(spec.CommitSHA), spec.Outcome, runID, now)
	if err != nil {
		return fmt.Errorf("insert receive event: %w", err)
	}
	return nil
}

func appendReceiveAudit(ctx context.Context, tx *sql.Tx, spec model.ReceiveEventSpec, runID string, now int64) error {
	details, err := json.Marshal(map[string]any{
		"event_id": spec.ID, "repo_id": spec.RepoID, "ref_kind": spec.RefKind, "ref": spec.Ref,
		"old_sha": spec.OldSHA, "object_sha": spec.ObjectSHA, "commit_sha": spec.CommitSHA,
		"outcome": spec.Outcome, "run_id": runID,
	})
	if err != nil {
		return fmt.Errorf("encode receive audit: %w", err)
	}
	if _, err := appendAuditAction(ctx, tx, model.AuditActionSpec{
		Actor: spec.Actor, Action: "push." + string(spec.RefKind) + "." + spec.Outcome,
		ResourceType: "receive", ResourceID: spec.ID, Details: string(details),
	}, now); err != nil {
		return fmt.Errorf("append receive audit: %w", err)
	}
	return nil
}

func scanReceiveEvent(row rowScanner) (model.ReceiveEvent, error) {
	var value model.ReceiveEvent
	var created int64
	if err := row.Scan(&value.ID, &value.Actor, &value.RepoID, &value.RefKind, &value.Ref,
		&value.OldSHA, &value.ObjectSHA, &value.CommitSHA, &value.Outcome, &value.RunID, &created); err != nil {
		return model.ReceiveEvent{}, err
	}
	value.CreatedAt = fromUnixNano(created)
	return value, nil
}

func sameStoredOID(first, second string) bool { return gitoid.Same(first, second) }
