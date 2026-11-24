package store

import (
	"context"
	"database/sql"
	"time"
)

type InstallationStats struct {
	Upstreams, Repositories int
	LastBackup              time.Time
}

// InstallationStats reads public counts and the last completed backup time in
// one snapshot. Audit details and catalog credentials never leave this query.
func (s *Store) InstallationStats(ctx context.Context) (InstallationStats, error) {
	var out InstallationStats
	var last sql.NullString
	err := s.readers.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM upstreams WHERE removed=0),
 (SELECT count(*) FROM repositories),
 (SELECT created_at FROM audit WHERE action='backup.completed' ORDER BY id DESC LIMIT 1)`).Scan(&out.Upstreams, &out.Repositories, &last)
	if err != nil {
		return out, databaseError(err)
	}
	if last.Valid {
		out.LastBackup, err = time.Parse(time.RFC3339Nano, last.String)
	}
	return out, err
}
