package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	resourceUsageMaxAge       = 90 * 24 * time.Hour
	resourceUsageRetainedRows = 100000
	resourceUsageSweepRows    = 1000
	resourceUsageDeleteBatch  = 100
)

// PruneResourceUsage removes the oldest observations beyond 90 days or the
// newest 100,000 rows globally. A sweep selects at most 1,000 candidates and
// deletes at most 100 per autocommit statement on its own connection. It never
// borrows the primary pool. SQLite still serializes file-wide writes; a 25ms
// busy timeout and one-second pass budget yield to other writers. Backlog and
// new inserts between sweeps may temporarily exceed the retention targets.
func (s *Store) PruneResourceUsage(ctx context.Context) (int64, error) {
	if s.readOnly || s.livePath == "" {
		return 0, ErrInvalidState
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var count int
	if err := s.reader().QueryRowContext(ctx, `SELECT count(*) FROM (SELECT rowid FROM step_resource_usage LIMIT ?)`, resourceUsageRetainedRows+resourceUsageSweepRows).Scan(&count); err != nil {
		return 0, err
	}
	excess := max(0, count-resourceUsageRetainedRows)
	rows, err := s.reader().QueryContext(ctx, `SELECT rowid,last_observed_at FROM step_resource_usage ORDER BY last_observed_at,rowid LIMIT ?`, resourceUsageSweepRows)
	if err != nil {
		return 0, err
	}
	var selected []resourceUsageCandidate
	cutoff := unixNano(s.now().Add(-resourceUsageMaxAge))
	for rows.Next() {
		var v resourceUsageCandidate
		if err := rows.Scan(&v.id, &v.at); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if len(selected) < excess || v.at < cutoff {
			selected = append(selected, v)
		} else {
			break
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil || len(selected) == 0 {
		return 0, err
	}
	info, err := os.Stat(s.livePath)
	if err != nil {
		return 0, err
	}
	if !os.SameFile(info, s.liveFile) {
		return 0, fmt.Errorf("%w: resource retention database identity changed", ErrInvalidState)
	}
	location, err := url.Parse(currentDatabaseDSN(s.livePath, "rw", false))
	if err != nil {
		return 0, err
	}
	query := location.Query()
	for _, pragma := range []string{"busy_timeout(25)", "foreign_keys(1)", "synchronous(FULL)"} {
		query.Add("_pragma", pragma)
	}
	location.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", location.String())
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	var deleted int64
	for len(selected) > 0 {
		batch := selected[:min(len(selected), resourceUsageDeleteBatch)]
		n, err := deleteResourceUsageBatch(ctx, db, batch)
		if err != nil {
			return deleted, err
		}
		deleted += n
		selected = selected[len(batch):]
	}
	return deleted, nil
}

type resourceUsageCandidate struct{ id, at int64 }

func deleteResourceUsageBatch(ctx context.Context, db *sql.DB, batch []resourceUsageCandidate) (int64, error) {
	if len(batch) == 0 {
		return 0, nil
	}
	if len(batch) > resourceUsageDeleteBatch {
		return 0, ErrInvalid
	}
	predicates := make([]string, 0, len(batch))
	args := make([]any, 0, len(batch)*2)
	for _, v := range batch {
		predicates = append(predicates, "(rowid=? AND last_observed_at=?)")
		args = append(args, v.id, v.at)
	}
	// Selection does not reserve a writer. A later observation must survive
	// even if it was selected as old before this DELETE obtains its lock.
	result, err := db.ExecContext(ctx, `DELETE FROM step_resource_usage WHERE `+strings.Join(predicates, " OR "), args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
