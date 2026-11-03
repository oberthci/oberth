package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func resourceRetentionSnapshot(t *testing.T, ctx context.Context, s *Store) []resourceUsageCandidate {
	t.Helper()
	rows, err := s.reader().QueryContext(ctx, `SELECT rowid,last_observed_at FROM step_resource_usage ORDER BY last_observed_at,rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var values []resourceUsageCandidate
	for rows.Next() {
		var v resourceUsageCandidate
		if err := rows.Scan(&v.id, &v.at); err != nil {
			t.Fatal(err)
		}
		values = append(values, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return values
}

// Both the live sweep and the deterministic cancellation regression use this
// assertion: persisted rows establish progress, including an unreported commit.
func checkResourceRetentionProgress(t *testing.T, expected, remaining []resourceUsageCandidate, total int, deleted int64, err error) int {
	t.Helper()
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("sweep/connection isolation", deleted, err)
	}
	if deleted < 0 || deleted > resourceUsageSweepRows {
		t.Fatal("sweep cap", deleted)
	}
	observedTotal := len(expected) - len(remaining)
	observedDeleted := int64(observedTotal - total)
	// SQLite may commit a DELETE before cancellation makes ExecContext
	// return a deadline error instead of its RowsAffected result. Only
	// that final batch can be missing from the acknowledged count.
	if observedDeleted < deleted || observedDeleted > resourceUsageSweepRows ||
		(err == nil && observedDeleted != deleted) || observedDeleted-deleted > resourceUsageDeleteBatch {
		t.Fatal("sweep count", observedDeleted, deleted, err)
	}
	if observedDeleted != deleted {
		t.Logf("deadline after committed batch: acknowledged=%d persisted=%d", deleted, observedDeleted)
	}
	// Verify every surviving row, including the rowid tie-breaker. The
	// persisted suffix is authoritative even when the last count is lost.
	if !slices.Equal(remaining, expected[observedTotal:]) {
		t.Fatal("partial sweep was not oldest first", len(remaining), observedTotal, deleted, err)
	}
	return observedTotal
}

type retentionDeadlineConnector struct {
	driver driver.Driver
	dsn    string
}

func (c retentionDeadlineConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &retentionDeadlineConn{Conn: conn}, nil
}

func (c retentionDeadlineConnector) Driver() driver.Driver { return c.driver }

type retentionDeadlineConn struct {
	driver.Conn
	deletes int
}

func (c *retentionDeadlineConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	result, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
	if err == nil && strings.HasPrefix(query, "DELETE FROM step_resource_usage") {
		c.deletes++
		if c.deletes == 3 {
			// Model SQLite's post-step cancellation check after a real autocommit,
			// without relying on a timer hitting the narrow cancellation window.
			return nil, context.DeadlineExceeded
		}
	}
	return result, err
}

func TestResourceRetentionOrderingAfterCommittedDeadline(t *testing.T) {
	s, run, at := resourceStoreFixture(t)
	old := at.Add(-resourceUsageMaxAge - time.Hour)
	const rows = 3*resourceUsageDeleteBatch + 2
	seedResourceRows(t, s, run.ID, old, rows)
	// Reverse insertion order with ties across each batch boundary.
	if _, err := s.db.Exec(`UPDATE step_resource_usage SET last_observed_at=?+(?-rowid)/3`, unixNano(old), rows); err != nil {
		t.Fatal(err)
	}
	expected := resourceRetentionSnapshot(t, t.Context(), s)
	if len(expected) != rows {
		t.Fatal("retention fixture row count", len(expected))
	}
	db := sql.OpenDB(retentionDeadlineConnector{driver: &sqlite.Driver{}, dsn: currentDatabaseDSN(s.livePath, "rw", false)})
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	var acknowledged int64
	var sweepErr error
	for start := 0; start < 3*resourceUsageDeleteBatch; start += resourceUsageDeleteBatch {
		n, err := deleteResourceUsageBatch(t.Context(), db, expected[start:start+resourceUsageDeleteBatch])
		if err != nil {
			sweepErr = err
			break
		}
		acknowledged += n
	}
	if acknowledged != 2*resourceUsageDeleteBatch || !errors.Is(sweepErr, context.DeadlineExceeded) {
		t.Fatal("missing committed-deadline injection", acknowledged, sweepErr)
	}
	remaining := resourceRetentionSnapshot(t, t.Context(), s)
	if len(remaining) != 2 {
		t.Fatal("third batch did not commit", len(remaining))
	}
	total := checkResourceRetentionProgress(t, expected, remaining, 0, acknowledged, sweepErr)
	// The next real sweep must drain the exact suffix with normal accounting.
	n, err := s.PruneResourceUsage(t.Context())
	total = checkResourceRetentionProgress(t, expected, resourceRetentionSnapshot(t, t.Context(), s), total, n, err)
	if total != rows {
		t.Fatal("backlog after committed deadline did not drain", total)
	}
}
