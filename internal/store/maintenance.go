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

const maintenanceColumns = `id, state, booked_by, reason, not_before, drain_timeout_ns,
 auto_end_ns, booked_version, drain_started_at, forced_at, forced_by, started_at,
 resumed_at, resumed_version, ended_at, ended_by, end_reason, created_at, updated_at`

func scanMaintenance(row rowScanner) (model.MaintenanceWindow, error) {
	var w model.MaintenanceWindow
	var notBefore, drain, autoEnd, created, updated int64
	var drainStarted, forced, started, resumed, ended sql.NullInt64
	err := row.Scan(&w.ID, &w.State, &w.BookedBy, &w.Reason, &notBefore, &drain,
		&autoEnd, &w.BookedVersion, &drainStarted, &forced, &w.ForcedBy, &started,
		&resumed, &w.ResumedVersion, &ended, &w.EndedBy, &w.EndReason, &created, &updated)
	if err != nil {
		return model.MaintenanceWindow{}, err
	}
	w.NotBefore, w.DrainTimeout, w.AutoEnd = fromUnixNano(notBefore), time.Duration(drain), time.Duration(autoEnd)
	w.DrainStartedAt, w.ForcedAt, w.StartedAt = nullableTime(drainStarted), nullableTime(forced), nullableTime(started)
	w.ResumedAt, w.EndedAt = nullableTime(resumed), nullableTime(ended)
	w.CreatedAt, w.UpdatedAt = fromUnixNano(created), fromUnixNano(updated)
	return w, nil
}

func maintenanceHold(w model.MaintenanceWindow) *model.MaintenanceHold {
	return &model.MaintenanceHold{WindowID: w.ID, State: w.State, BookedBy: w.BookedBy, Reason: w.Reason, NotBefore: w.NotBefore}
}

func currentMaintenanceTx(ctx context.Context, tx *sql.Tx) (model.MaintenanceWindow, error) {
	w, err := scanMaintenance(tx.QueryRowContext(ctx, `SELECT `+maintenanceColumns+` FROM maintenance_windows WHERE open_singleton=1`))
	if errors.Is(err, sql.ErrNoRows) {
		return w, fmt.Errorf("%w: open maintenance window", ErrNotFound)
	}
	return w, err
}

func (s *Store) CurrentMaintenance(ctx context.Context) (model.MaintenanceWindow, error) {
	w, err := scanMaintenance(s.db.QueryRowContext(ctx, `SELECT `+maintenanceColumns+` FROM maintenance_windows WHERE open_singleton=1`))
	if errors.Is(err, sql.ErrNoRows) {
		return w, fmt.Errorf("%w: open maintenance window", ErrNotFound)
	}
	return w, err
}

func (s *Store) ListMaintenance(ctx context.Context, limit int) ([]model.MaintenanceWindow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+maintenanceColumns+` FROM maintenance_windows ORDER BY created_at DESC, id DESC LIMIT ?`, pageLimit(limit))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []model.MaintenanceWindow
	for rows.Next() {
		w, err := scanMaintenance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func maintenanceProgressTx(ctx context.Context, tx *sql.Tx, w model.MaintenanceWindow) (model.MaintenanceProgress, error) {
	p := model.MaintenanceProgress{Window: w}
	err := tx.QueryRowContext(ctx, `SELECT
 (SELECT COUNT(*) FROM runs WHERE status='running'),
 (SELECT COUNT(*) FROM runs WHERE status='running' AND (release=1 OR concurrency_group!='' OR EXISTS (
   SELECT 1 FROM publications WHERE publications.run_id=runs.id AND publications.status='pending'))),
 (SELECT COUNT(*) FROM runs WHERE status='queued'),
 (SELECT COUNT(*) FROM run_cancellations WHERE completed_at IS NULL AND job_name!='')`).Scan(
		&p.RunningRuns, &p.ProtectedRuns, &p.HeldRuns, &p.PendingCancellations)
	if err != nil {
		return p, err
	}
	if !w.State.Holds() {
		p.HeldRuns = 0
	}
	if w.DrainStartedAt != nil {
		deadline := w.DrainStartedAt.Add(w.DrainTimeout)
		p.DrainDeadline = &deadline
	}
	if w.ResumedAt != nil && w.AutoEnd > 0 {
		at := w.ResumedAt.Add(w.AutoEnd)
		p.AutoEndAt = &at
	}
	return p, nil
}

func (s *Store) MaintenanceProgress(ctx context.Context) (model.MaintenanceProgress, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return model.MaintenanceProgress{}, err
	}
	defer func() { _ = tx.Rollback() }()
	w, err := currentMaintenanceTx(ctx, tx)
	if err != nil {
		return model.MaintenanceProgress{}, err
	}
	return maintenanceProgressTx(ctx, tx, w)
}

func (s *Store) maintenanceAudit(ctx context.Context, tx *sql.Tx, actor, action, id string, details any, now int64) error {
	body, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{
		Actor: actor, Action: "maintenance." + action, ResourceType: "maintenance_window",
		ResourceID: id, Details: string(body),
	}, now)
	return err
}

func (s *Store) BookMaintenance(ctx context.Context, spec model.MaintenanceSpec) (model.MaintenanceWindow, error) {
	if strings.TrimSpace(spec.BookedBy) == "" || len(spec.Reason) > 512 || spec.DrainTimeout <= 0 || spec.AutoEnd < 0 {
		return model.MaintenanceWindow{}, fmt.Errorf("%w: invalid maintenance specification", ErrInvalid)
	}
	now := s.now().UTC()
	if spec.NotBefore.IsZero() {
		spec.NotBefore = now
	}
	id, err := randomID()
	if err != nil {
		return model.MaintenanceWindow{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.MaintenanceWindow{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var open bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM maintenance_windows WHERE open_singleton=1)`).Scan(&open); err != nil {
		return model.MaintenanceWindow{}, err
	}
	if open {
		return model.MaintenanceWindow{}, fmt.Errorf("%w: maintenance window already open", ErrInvalidState)
	}
	state := model.MaintenanceBooked
	var drainStarted any
	if !spec.NotBefore.After(now) {
		state, drainStarted = model.MaintenanceDraining, unixNano(now)
	}
	w, err := scanMaintenance(tx.QueryRowContext(ctx, `INSERT INTO maintenance_windows
 (id,state,open_singleton,booked_by,reason,not_before,drain_timeout_ns,auto_end_ns,booked_version,drain_started_at,created_at,updated_at)
 VALUES(?,?,1,?,?,?,?,?,?,?,?,?) RETURNING `+maintenanceColumns,
		id, state, spec.BookedBy, spec.Reason, unixNano(spec.NotBefore), int64(spec.DrainTimeout), int64(spec.AutoEnd), spec.BookedVersion, drainStarted, unixNano(now), unixNano(now)))
	if err != nil {
		return model.MaintenanceWindow{}, err
	}
	if err := s.maintenanceAudit(ctx, tx, spec.BookedBy, "book", id, map[string]any{
		"not_before": spec.NotBefore, "drain_timeout_ns": int64(spec.DrainTimeout), "auto_end_ns": int64(spec.AutoEnd),
		"reason": spec.Reason, "booked_version": spec.BookedVersion,
	}, unixNano(now)); err != nil {
		return model.MaintenanceWindow{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.MaintenanceWindow{}, err
	}
	return w, nil
}

func maintenanceTarget(ctx context.Context, tx *sql.Tx, id string, allowed ...model.MaintenanceState) (model.MaintenanceWindow, error) {
	w, err := currentMaintenanceTx(ctx, tx)
	if err != nil {
		return w, err
	}
	if id == "" || w.ID != id {
		return w, fmt.Errorf("%w: maintenance window", ErrNotFound)
	}
	for _, state := range allowed {
		if w.State == state {
			return w, nil
		}
	}
	return w, fmt.Errorf("%w: maintenance window is %s", ErrInvalidState, w.State)
}

func (s *Store) CancelMaintenance(ctx context.Context, id, actor string) error {
	if strings.TrimSpace(actor) == "" {
		return fmt.Errorf("%w: actor is required", ErrInvalid)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := maintenanceTarget(ctx, tx, id, model.MaintenanceBooked, model.MaintenanceDraining); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE maintenance_windows SET state='aborted',open_singleton=NULL,ended_at=?,ended_by=?,end_reason='canceled',updated_at=? WHERE id=?`, now, actor, now, id); err != nil {
		return err
	}
	if err := s.maintenanceAudit(ctx, tx, actor, "cancel", id, map[string]any{}, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) EndMaintenance(ctx context.Context, id, actor, reason string) error {
	if strings.TrimSpace(actor) == "" || (reason != "ended" && reason != "auto_end") {
		return fmt.Errorf("%w: actor or end reason", ErrInvalid)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	w, err := maintenanceTarget(ctx, tx, id, model.MaintenanceActive)
	if err != nil {
		return err
	}
	p, err := maintenanceProgressTx(ctx, tx, w)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE maintenance_windows SET state='ended',open_singleton=NULL,ended_at=?,ended_by=?,end_reason=?,updated_at=? WHERE id=?`, now, actor, reason, now, id); err != nil {
		return err
	}
	if err := s.maintenanceAudit(ctx, tx, actor, "end", id, map[string]any{"held_runs_released": p.HeldRuns, "end_reason": reason}, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) BeginMaintenanceDrain(ctx context.Context, id string, at time.Time) error {
	now := unixNano(at)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	w, err := maintenanceTarget(ctx, tx, id, model.MaintenanceBooked)
	if err != nil {
		return err
	}
	if at.Before(w.NotBefore) {
		return fmt.Errorf("%w: maintenance is not due", ErrInvalidState)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE maintenance_windows SET state='draining',drain_started_at=?,updated_at=? WHERE id=?`, now, now, id); err != nil {
		return err
	}
	if err := s.maintenanceAudit(ctx, tx, model.MaintenanceActor, "drain", id, map[string]any{}, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ActivateMaintenanceIfQuiet(ctx context.Context, id string, at time.Time) (bool, model.MaintenanceProgress, error) {
	now := unixNano(at)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, model.MaintenanceProgress{}, err
	}
	defer func() { _ = tx.Rollback() }()
	w, err := maintenanceTarget(ctx, tx, id, model.MaintenanceDraining)
	if err != nil {
		return false, model.MaintenanceProgress{}, err
	}
	p, err := maintenanceProgressTx(ctx, tx, w)
	if err != nil {
		return false, p, err
	}
	if p.RunningRuns != 0 || p.PendingCancellations != 0 {
		return false, p, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE maintenance_windows SET state='active',started_at=?,updated_at=? WHERE id=?`, now, now, id); err != nil {
		return false, p, err
	}
	drainSeconds := 0.0
	if w.DrainStartedAt != nil {
		drainSeconds = at.Sub(*w.DrainStartedAt).Seconds()
	}
	if err := s.maintenanceAudit(ctx, tx, model.MaintenanceActor, "activate", id, map[string]any{"forced": w.ForcedAt != nil, "drain_seconds": drainSeconds}, now); err != nil {
		return false, p, err
	}
	if err := tx.Commit(); err != nil {
		return false, p, err
	}
	w.State, w.StartedAt, w.UpdatedAt = model.MaintenanceActive, &at, at
	p.Window = w
	return true, p, nil
}

func (s *Store) AbortMaintenanceIfExpired(ctx context.Context, id string, at time.Time) (bool, error) {
	now := unixNano(at)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	w, err := maintenanceTarget(ctx, tx, id, model.MaintenanceDraining)
	if err != nil {
		return false, err
	}
	if w.ForcedAt != nil || w.DrainStartedAt == nil || at.Before(w.DrainStartedAt.Add(w.DrainTimeout)) {
		return false, nil
	}
	p, err := maintenanceProgressTx(ctx, tx, w)
	if err != nil {
		return false, err
	}
	if p.RunningRuns == 0 && p.PendingCancellations == 0 {
		return false, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM runs WHERE status='running' ORDER BY queue_sequence`)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			_ = rows.Close()
			return false, err
		}
		ids = append(ids, runID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, err
	}
	_ = rows.Close()
	if _, err := tx.ExecContext(ctx, `UPDATE maintenance_windows SET state='aborted',open_singleton=NULL,ended_at=?,ended_by=?,end_reason='drain_timeout',updated_at=? WHERE id=?`, now, model.MaintenanceActor, now, id); err != nil {
		return false, err
	}
	if err := s.maintenanceAudit(ctx, tx, model.MaintenanceActor, "abort", id, map[string]any{"running_run_ids": ids}, now); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) ResumeMaintenance(ctx context.Context, id, version string, at time.Time) error {
	now := unixNano(at)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	w, err := maintenanceTarget(ctx, tx, id, model.MaintenanceActive)
	if err != nil {
		return err
	}
	if w.ResumedAt != nil {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE maintenance_windows SET resumed_at=?,resumed_version=?,updated_at=? WHERE id=?`, now, version, now, id); err != nil {
		return err
	}
	if err := s.maintenanceAudit(ctx, tx, model.MaintenanceActor, "resume", id, map[string]any{"resumed_version": version}, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ForceMaintenance(ctx context.Context, id, actor string) ([]model.Run, error) {
	if strings.TrimSpace(actor) == "" {
		return nil, fmt.Errorf("%w: actor is required", ErrInvalid)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	w, err := maintenanceTarget(ctx, tx, id, model.MaintenanceDraining)
	if err != nil {
		return nil, err
	}
	if w.ForcedAt != nil {
		return nil, fmt.Errorf("%w: maintenance was already forced", ErrInvalidState)
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+runColumns+` FROM runs WHERE status='running' ORDER BY queue_sequence`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var running []model.Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		running = append(running, run)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	var forced []model.Run
	var protected []string
	for _, run := range running {
		var publication bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM publications WHERE run_id=? AND status='pending')`, run.ID).Scan(&publication); err != nil {
			return nil, err
		}
		if run.Release || run.ConcurrencyGroup != "" || publication {
			protected = append(protected, run.ID)
			continue
		}
		finished, err := scanRun(tx.QueryRowContext(ctx, `UPDATE runs SET status='interrupted',phase='interrupted',reason='maintenance',error=?,finished_at=?,updated_at=? WHERE id=? AND status='running' RETURNING `+runColumns,
			"interrupted by maintenance window "+id, now, now, run.ID))
		if err != nil {
			return nil, err
		}
		if finished.JobName != "" {
			_, err = tx.ExecContext(ctx, `INSERT INTO run_cancellations(run_id,job_name,superseded_by,reason,created_at) VALUES(?,?,'','owner_restart',?) ON CONFLICT(run_id) DO UPDATE SET job_name=excluded.job_name,superseded_by='',reason='owner_restart',completed_at=NULL`, finished.ID, finished.JobName, now)
			if err != nil {
				return nil, err
			}
		}
		if finished.Trigger == "promotion" {
			promotion, err := promotionByRunTx(ctx, tx, finished.ID)
			if err != nil {
				return nil, err
			}
			promotion, err = finishPromotionTx(ctx, tx, promotion, model.PromotionInterrupted, finished.ID, finished.Error, now)
			if err != nil {
				return nil, err
			}
			if err := s.projectPromotionIssue(ctx, tx, promotion, now); err != nil {
				return nil, err
			}
		} else if err := s.projectRunIssue(ctx, tx, finished, "", now); err != nil {
			return nil, err
		}
		forced = append(forced, finished)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE maintenance_windows SET forced_at=?,forced_by=?,updated_at=? WHERE id=?`, now, actor, now, id); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(forced))
	for _, run := range forced {
		ids = append(ids, run.ID)
	}
	if err := s.maintenanceAudit(ctx, tx, actor, "force", id, map[string]any{"interrupted_run_ids": ids, "protected_run_ids": protected}, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return forced, nil
}
