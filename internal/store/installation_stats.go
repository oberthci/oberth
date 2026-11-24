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

const installationCountsSQL = `SELECT
 (SELECT count(*) FROM upstreams WHERE removed=0),
 (SELECT count(*) FROM repositories)`

// InstallationStats reads catalog counts and the committed backup snapshot.
// Request-time observation never searches the audit history.
func (s *Store) InstallationStats(ctx context.Context) (InstallationStats, error) {
	var out InstallationStats
	err := s.readers.QueryRowContext(ctx, installationCountsSQL).Scan(&out.Upstreams, &out.Repositories)
	if err != nil {
		return out, databaseError(err)
	}
	s.backupMu.RLock()
	out.LastBackup = s.lastBackup
	s.backupMu.RUnlock()
	return out, nil
}

// Hydrate once when opening existing history. Later changes use the serialized
// committed audit receipt, with no database migration or background worker.
func (s *Store) loadLastBackup(ctx context.Context) error {
	var last sql.NullString
	err := s.readers.QueryRowContext(ctx, `SELECT (SELECT created_at FROM audit WHERE action='backup.completed' ORDER BY id DESC LIMIT 1)`).Scan(&last)
	if err != nil {
		return databaseError(err)
	}
	if last.Valid {
		s.lastBackup, err = time.Parse(timestampFormat, last.String)
	}
	return err
}

func backupAtHead(ctx context.Context, tx *sql.Tx) (time.Time, error) {
	var timestamp string
	if err := tx.QueryRowContext(ctx, `SELECT created_at FROM audit WHERE id=(SELECT id FROM audit_head WHERE singleton=1)`).Scan(&timestamp); err != nil {
		return time.Time{}, databaseError(err)
	}
	return time.Parse(timestampFormat, timestamp)
}
