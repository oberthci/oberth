package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

// RetainSchedule saves a snapshot before native publication without evicting one.
func (s *Store) RetainSchedule(ctx context.Context, p *admissioncontract.PreparedAdmission) error {
	if p == nil {
		return refuse(invalid, nil)
	}
	v := p.View()
	if !validScheduleAdmission(v) {
		return refuse(invalid, nil)
	}
	body, err := json.Marshal(p)
	if err != nil {
		return refuse(invalid, err)
	}
	return s.observe(ctx, func(tx *sql.Tx) error {
		id, err := repositoryNumber(ctx, tx, v.Repository)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO schedule_admissions(repo_id,entry,digest,value) VALUES(?,?,?,?) ON CONFLICT(digest) DO NOTHING`, id, v.ScheduleEntry, v.Digest, body)
		return databaseError(err)
	})
}

// PublishSchedule keeps the current and previous successfully published snapshots.
// The native parent supplies both digests, including an unchanged publication.
func (s *Store) PublishSchedule(ctx context.Context, id int64, entry string, keep []string) error {
	if len(keep) != 2 || keep[0] == "" {
		return refuse(invalid, nil)
	}
	current, previous := keep[0], keep[1]
	return s.observe(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE schedule_admissions SET rank=2 WHERE repo_id=? AND entry=? AND digest=?`, id, entry, current)
		if err != nil {
			return databaseError(err)
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return refuse(missing, err)
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM schedule_admissions WHERE repo_id=? AND entry=? AND digest NOT IN (?,?)`, id, entry, current, previous); err != nil {
			return databaseError(err)
		}
		_, err = tx.ExecContext(ctx, `UPDATE schedule_admissions SET rank=1 WHERE repo_id=? AND entry=? AND digest<>?`, id, entry, current)
		return databaseError(err)
	})
}

func validScheduleAdmission(v admissioncontract.PreparedView) bool {
	return v.RunID == "" && v.Trigger == model.TriggerSchedule && v.ScheduleEntry != ""
}

// ScheduleAdmission distinguishes an unseen entry from a superseded digest.
// Existing rows are self-checked; the child consumer verifies their run facts.
func (s *Store) ScheduleAdmission(ctx context.Context, id int64, entry, digest string) (admissioncontract.PreparedView, error) {
	var body []byte
	var repository, storedEntry string
	var v admissioncontract.PreparedView
	tx, err := s.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return v, databaseError(err)
	}
	defer func() { _ = tx.Rollback() }()
	err = tx.QueryRowContext(ctx, `SELECT a.value,r.canonical,a.entry FROM schedule_admissions a JOIN repositories r ON r.id=a.repo_id WHERE a.digest=?`, digest).Scan(&body, &repository, &storedEntry)
	if errors.Is(err, sql.ErrNoRows) {
		var retained bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schedule_admissions WHERE repo_id=? AND entry=?)`, id, entry).Scan(&retained); err != nil {
			return v, databaseError(err)
		}
		if retained {
			return v, refuse(scheduleAdmissionExpired, stateport.ScheduleAdmissionExpiredError{})
		}
		return v, refuse(missing, nil)
	}
	if err != nil {
		return v, databaseError(err)
	}
	if err := json.Unmarshal(body, &v); err != nil || v.Digest != digest || v.Repository != repository || v.ScheduleEntry != storedEntry {
		return v, refuse(corrupt, err)
	}
	return v, nil
}

// ForgetSchedule removes snapshots only after the native parent is removed.
func (s *Store) ForgetSchedule(ctx context.Context, id int64, entry string) error {
	return s.observe(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM schedule_admissions WHERE repo_id=? AND entry=?`, id, entry)
		return databaseError(err)
	})
}

// PruneSchedules also removes unused snapshots left by a failed parent apply.
// The caller supplies entries from a complete declaration observation.
func (s *Store) PruneSchedules(ctx context.Context, id int64, keep []string) error {
	query := `DELETE FROM schedule_admissions WHERE repo_id=?`
	args := []any{id}
	if len(keep) > 0 {
		query += ` AND entry NOT IN (` + strings.TrimSuffix(strings.Repeat("?,", len(keep)), ",") + `)`
		for _, entry := range keep {
			args = append(args, entry)
		}
	}
	return s.observe(ctx, func(tx *sql.Tx) error { _, err := tx.ExecContext(ctx, query, args...); return databaseError(err) })
}
