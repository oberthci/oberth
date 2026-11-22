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
	previous, err := projectedSequence(ctx, tx, r.Repo, r.Ref)
	if err != nil {
		return err
	}
	if previous >= r.Sequence {
		return unchanged{}
	}
	return s.recordProjection(ctx, tx, r)
}

func projectedSequence(ctx context.Context, tx *sql.Tx, repo, ref string) (int64, error) {
	var previous int64
	err := tx.QueryRowContext(ctx, `SELECT sequence FROM ci_projection WHERE repo=? AND ref=?`, repo, ref).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, databaseError(err)
	}
	return previous, nil
}

// projectPublished makes a delivered publication's passing run the CI result
// of the published ref, ordered right after the runs that existed when its
// first push attempt started: the ref is now at the commit that run tested,
// and those runs tested older commits. A run created after that start, which
// may have tested the new head, keeps the issue if it was counted already.
func (s *Store) projectPublished(ctx context.Context, tx *sql.Tx, p model.Publication) error {
	r, err := loadRun(ctx, tx, p.RunID)
	if err != nil {
		return err
	}
	if r.Status != stepmodel.RunPassed || r.Origin.HasAssistance() || r.CommitSHA == nil || p.CommitSHA == nil || *r.CommitSHA != *p.CommitSHA {
		return refuse(conflict, nil)
	}
	var counted bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM issue_work_receipts WHERE run_id=?)`, r.ID).Scan(&counted); err != nil {
		return databaseError(err)
	}
	if counted {
		return nil
	}
	r.Ref, r.Sequence = p.Ref, p.CIAfter
	previous, err := projectedSequence(ctx, tx, r.Repo, r.Ref)
	if err != nil || previous > r.Sequence {
		return err
	}
	return s.recordProjection(ctx, tx, r)
}

func (s *Store) recordProjection(ctx context.Context, tx *sql.Tx, r stateport.Run) error {
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
