package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/oberthci/oberth/internal/stateport"
)

const historyColumns = `repo_id,sequence,revision,summary,origin,release,step_count,steps_planned_exact,
steps_pending,steps_queued,steps_running,steps_passed,steps_failed,steps_skipped,steps_unknown,has_admission`

// Runs filters and selects latest groups before applying the descending cursor.
func (s *Store) Runs(ctx context.Context, filter stateport.RunFilter) ([]stateport.Run, error) {
	if err := validateRunFilter(filter); err != nil {
		return nil, err
	}
	tx, err := s.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, databaseError(err)
	}
	defer func() { _ = tx.Rollback() }()
	before := filter.Before
	if filter.Cursor != "" {
		sequence, err := runCursor(ctx, tx, filter)
		if err != nil {
			return nil, err
		}
		if before == 0 || sequence < before {
			before = sequence
		}
	}
	query, args := runListQuery(filter, before)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, databaseError(err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]stateport.Run, 0)
	for rows.Next() {
		r, err := scanHistory(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, databaseError(rows.Err())
}

func runCursor(ctx context.Context, tx *sql.Tx, filter stateport.RunFilter) (int64, error) {
	predicate, args := scopePredicate("repositories.canonical", filter.Repositories)
	var sequence int64
	err := tx.QueryRowContext(ctx, `SELECT runs.sequence FROM runs JOIN repositories ON repositories.id=runs.repo_id
WHERE `+predicate+` AND runs.id=?`, append(args, filter.Cursor)...).Scan(&sequence)
	return sequence, databaseError(err)
}

func runListQuery(f stateport.RunFilter, before int64) (string, []any) {
	predicate, args := runPredicate(f)
	rank := ""
	if f.Latest || f.LatestRepository {
		partition := "runs.repo_id,runs.ref"
		if f.LatestRepository {
			partition = "runs.repo_id"
		}
		rank = `,row_number() OVER(PARTITION BY ` + partition + ` ORDER BY runs.sequence DESC) AS rank`
	}
	query := `WITH filtered AS(SELECT runs.repo_id,runs.sequence,runs.revision,runs.summary,runs.origin,runs.release,runs.step_count,
runs.steps_planned_exact,runs.steps_pending,runs.steps_queued,runs.steps_running,runs.steps_passed,runs.steps_failed,
runs.steps_skipped,runs.steps_unknown,runs.admission IS NOT NULL AS has_admission` + rank + `
FROM runs JOIN repositories ON repositories.id=runs.repo_id WHERE ` + predicate + `) SELECT ` + historyColumns + ` FROM filtered WHERE 1`
	if rank != "" {
		query += ` AND rank=1`
	}
	if before > 0 {
		query += ` AND sequence<?`
		args = append(args, before)
	}
	query += ` ORDER BY sequence DESC LIMIT ?`
	return query, append(args, f.Limit)
}

func runPredicate(f stateport.RunFilter) (string, []any) {
	predicate, args := scopePredicate("repositories.canonical", f.Repositories)
	for _, v := range []struct {
		column  string
		value   any
		present bool
	}{
		{"runs.status", f.Status, f.Status != ""},
		{"json_extract(runs.summary,'$.commit_sha')", f.CommitSHA, f.CommitSHA != ""},
		{"runs.ref", f.Ref, f.Ref != ""},
		{"json_extract(runs.origin,'$.actor')", f.Actor, f.Actor != ""},
		{"json_extract(runs.origin,'$.trigger')", f.Trigger, f.Trigger != ""},
	} {
		if v.present {
			predicate += " AND " + v.column + "=?"
			args = append(args, v.value)
		}
	}
	if f.RefGlob != "" {
		predicate += ` AND runs.ref GLOB ?`
		args = append(args, f.RefGlob)
	}
	return predicate, args
}

func scanHistory(rows *sql.Rows) (stateport.Run, error) {
	var r stateport.Run
	var summary, origin string
	err := rows.Scan(&r.RepositoryID, &r.Sequence, &r.Revision, &summary, &origin, &r.Release, &r.Total, &r.Exact, &r.Pending,
		&r.Queued, &r.Running, &r.Passed, &r.Failed, &r.Skipped, &r.Unknown, &r.HasAdmission)
	if err != nil {
		return r, databaseError(err)
	}
	counts := r.LogicalStepCounts
	if json.Unmarshal([]byte(summary), &r.RunSummary) != nil || json.Unmarshal([]byte(origin), &r.Origin) != nil {
		return r, refuse(corrupt, nil)
	}
	r.LogicalStepCounts = counts
	return r, nil
}

func validateRunFilter(f stateport.RunFilter) error {
	if f.Limit < 1 || f.Limit > stateport.RunPageLimit || f.Before < 0 || (f.Status != "" && !f.Status.Valid()) {
		return refuse(invalid, nil)
	}
	if f.Trigger != "" && !f.Trigger.Valid() {
		return refuse(invalidTrigger, nil)
	}
	if f.RefGlob != "" && !stateport.ValidRefGlob(f.RefGlob) {
		return refuse(invalidGlob, nil)
	}
	if strings.IndexByte(f.Cursor, 0) >= 0 {
		return refuse(invalid, nil)
	}
	return nil
}
