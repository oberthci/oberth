package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"time"
)

// openAuthReaders isolates SSH identity reads from the serialized writer.
// Resolve the actual SQLite filename rather than reusing caller DSN options:
// auth must never inherit _txlock=immediate, immutable, or a writable mode.
func (s *Store) openAuthReaders(ctx context.Context, busyTimeout time.Duration) error {
	var sequence int
	var name, path string
	if err := s.db.QueryRowContext(ctx, `SELECT seq, name, file FROM pragma_database_list WHERE name = 'main'`).Scan(&sequence, &name, &path); err != nil {
		return fmt.Errorf("locate SSH identity database: %w", err)
	}
	if path == "" {
		return fmt.Errorf("%w: SSH identity readers require a file-backed WAL database", ErrInvalid)
	}
	if busyTimeout <= 0 {
		busyTimeout = defaultBusyTimeout
	}
	location, err := url.Parse(currentDatabaseDSN(path, "ro", false))
	if err != nil {
		return fmt.Errorf("parse SSH identity database location: %w", err)
	}
	query := location.Query()
	query.Add("_pragma", "query_only(1)")
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	location.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", location.String())
	if err != nil {
		return fmt.Errorf("open SSH identity readers: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return fmt.Errorf("connect SSH identity readers: %w", err)
	}
	s.authDB = db
	return nil
}
