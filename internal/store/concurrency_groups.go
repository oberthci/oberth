package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

// SetRunConcurrencyGroup records immutable YAML before admission. Omitting the
// limit preserves the old caller's default of one. The latest accepted build
// and release declarations remain durable even after their runs finish.
func (s *Store) SetRunConcurrencyGroup(ctx context.Context, id, group string, limits ...int) error {
	if strings.TrimSpace(id) == "" || len(limits) > 1 {
		return fmt.Errorf("%w: invalid concurrency declaration", ErrInvalid)
	}
	limit := 1
	if len(limits) == 1 {
		limit = limits[0]
	}
	if group == "" && len(limits) == 0 {
		return nil
	}
	if limit < 1 || int64(limit) > 2147483647 || group != "" && !validConcurrencyGroup(group) {
		return fmt.Errorf("%w: invalid concurrency group or limit", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	run, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if run.ConcurrencyGroup != "" && (run.ConcurrencyGroup != group || run.ConcurrencyLimit != limit) {
		return fmt.Errorf("%w: run %s already records concurrency group %q limit %d", ErrInvalidState, id, run.ConcurrencyGroup, run.ConcurrencyLimit)
	}
	if !run.Status.Active() {
		return nil
	}
	if group != "" {
		var other int
		err = tx.QueryRowContext(ctx, `SELECT group_limit FROM concurrency_group_declarations WHERE group_name=? AND group_limit<>? AND NOT (repo_id=? AND release=?) LIMIT 1`, group, limit, run.RepoID, run.Release).Scan(&other)
		if err == nil {
			return fmt.Errorf("%w: concurrency group %q limit %d conflicts with an accepted YAML declaration of limit %d", ErrInvalid, group, limit, other)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		err = tx.QueryRowContext(ctx, `SELECT concurrency_limit FROM runs WHERE concurrency_group=? AND concurrency_limit<>? AND status IN ('queued','running') AND id<>? LIMIT 1`, group, limit, id).Scan(&other)
		if err == nil {
			return fmt.Errorf("%w: concurrency group %q limit %d conflicts with an unfinished run's limit %d", ErrInvalid, group, limit, other)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO concurrency_group_declarations(repo_id,release,group_name,group_limit) VALUES(?,?,?,?) ON CONFLICT(repo_id,release) DO UPDATE SET group_name=excluded.group_name,group_limit=excluded.group_limit`, run.RepoID, run.Release, group, limit)
	} else {
		_, err = tx.ExecContext(ctx, `DELETE FROM concurrency_group_declarations WHERE repo_id=? AND release=?`, run.RepoID, run.Release)
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE runs SET concurrency_group=?,concurrency_limit=?,updated_at=? WHERE id=?`, group, limit, unixNano(s.now()), id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ConcurrencyGroups lists accepted YAML definitions, including idle groups.
// Live holders and waiters are supplied by the scheduler, never inferred from
// the durable running status (which also includes unstarted claims).
func (s *Store) ConcurrencyGroups(ctx context.Context) ([]model.AdmissionGroup, error) {
	rows, err := s.reader().QueryContext(ctx, `SELECT group_name,min(group_limit) FROM concurrency_group_declarations GROUP BY group_name ORDER BY group_name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	groups := []model.AdmissionGroup{}
	for rows.Next() {
		g := model.AdmissionGroup{Holders: []string{}, Waiting: []string{}}
		if err := rows.Scan(&g.Name, &g.Limit); err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}
