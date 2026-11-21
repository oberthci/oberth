package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// ProjectCI persists a monotonic repair projection from typed terminal evidence.
// Assisted runs never supply green evidence; receipts keep delayed outcomes inert.
func (s *Store) ProjectCI(ctx context.Context, action stateport.Action, runID string) error {
	return s.observe(ctx, func(tx *sql.Tx) error {
		r, err := loadRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		return s.projectCI(ctx, tx, r)
	}, action.Events...)
}

func (s *Store) applyCIOutcome(ctx context.Context, tx *sql.Tx, r stateport.Run) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM issues WHERE repo=? AND json_extract(value,'$.kind')='ci' AND json_extract(value,'$.branch')=?
ORDER BY id DESC LIMIT 1`, r.Repo, r.Ref).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, databaseError(err)
	}
	issue := model.Issue{}
	if id > 0 {
		issue, err = readIssue(ctx, tx, id)
		if err != nil {
			return 0, err
		}
	} else if r.Status == stepmodel.RunPassed {
		return 0, nil
	}
	issue = issue.WithCIOutcome(model.CIIssueOutcome{Repo: r.Repo, Ref: r.Ref, RunID: r.ID,
		Body: encode(r.Failure), Passed: r.Status == stepmodel.RunPassed, At: s.options.Clock().UTC()})
	err = saveIssue(ctx, tx, &issue, id == 0)
	return issue.ID, err
}

func (s *Store) projectCI(ctx context.Context, tx *sql.Tx, r stateport.Run) error {
	if !terminal(r.Status) || r.Origin.HasAssistance() {
		return refuse(conflict, nil)
	}
	var previous int64
	err := tx.QueryRowContext(ctx, `SELECT sequence FROM ci_projection WHERE repo=? AND ref=?`, r.Repo, r.Ref).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return databaseError(err)
	}
	if previous >= r.Sequence {
		return unchanged{}
	}
	id, err := s.applyCIOutcome(ctx, tx, r)
	if err != nil {
		return err
	}
	var issueID any
	if id > 0 {
		issueID = id
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO issue_work_receipts VALUES(?,?,?,?)`, r.ID, issueID, r.Sequence, r.Status); err != nil {
		return databaseError(err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO ci_projection VALUES(?,?,?,?) ON CONFLICT(repo,ref) DO UPDATE SET sequence=excluded.sequence,
run_id=excluded.run_id`, r.Repo, r.Ref, r.Sequence, r.ID)
	return databaseError(err)
}
