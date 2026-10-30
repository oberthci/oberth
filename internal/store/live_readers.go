package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"time"
)

const (
	liveReadConnections  = 8
	auditReadConnections = 2
)

// Initialize only after migrations and startup recovery, before publishing the
// Store to callers. Authentication, idle and readiness have their own pools so
// expensive list queries cannot consume their reserved reader connections.
func (s *Store) configureReadPool(ctx context.Context) error {
	db, err := s.openLiveReaders(ctx, liveReadConnections)
	if err != nil {
		return err
	}
	s.readDB = db
	// Full-chain and witness scans must not occupy hot mapping/list slots.
	auditDB, err := s.openLiveReaders(ctx, auditReadConnections)
	if err != nil {
		return err
	}
	s.auditReadDB = auditDB
	return nil
}

func (s *Store) auditReader() *sql.DB {
	if s.auditReadDB != nil {
		return s.auditReadDB
	}
	return s.reader()
}

func (s *Store) reader() *sql.DB {
	if s.readDB != nil {
		return s.readDB
	}
	// Private immutable inspection handles, startup and internal fixtures keep
	// their original connection policy. Live Stores always have a reader pool.
	return s.db
}

// configureLiveReaders resolves the actual filename once at startup. Audit
// readers and SSH identity readers share this DSN builder; opening a reader
// never needs to acquire the primary connection held by another transaction.
func (s *Store) configureLiveReaders(ctx context.Context, busyTimeout time.Duration) error {
	var path string
	if err := s.db.QueryRowContext(ctx, `SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		return fmt.Errorf("locate live reader database: %w", err)
	}
	if path == "" {
		return fmt.Errorf("%w: live readers require a file-backed WAL database", ErrInvalid)
	}
	if busyTimeout <= 0 {
		busyTimeout = defaultBusyTimeout
	}
	location, err := url.Parse(currentDatabaseDSN(path, "ro", false))
	if err != nil {
		return fmt.Errorf("parse live reader database: %w", err)
	}
	query := location.Query()
	query.Add("_pragma", "query_only(1)")
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	location.RawQuery = query.Encode()
	s.liveReaderDSN = location.String()
	s.liveBusyTimeout = busyTimeout
	s.livePath = path
	s.liveFile, err = os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat live reader database: %w", err)
	}
	return nil
}

// openLiveReaders is the common read-only WAL pool opener. It inherits neither
// caller DSN transaction mode nor immutable settings. Every pool connection
// gets the configured busy timeout and query_only pragma through the DSN.
func (s *Store) openLiveReaders(ctx context.Context, maxConnections int) (*sql.DB, error) {
	return s.openLiveReadersWithDSN(ctx, maxConnections, s.liveReaderDSN)
}

func (s *Store) openLiveReadersWithDSN(ctx context.Context, maxConnections int, dsn string) (*sql.DB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("%w: live readers are not configured", ErrInvalidState)
	}
	if maxConnections <= 0 {
		return nil, fmt.Errorf("%w: reader connection limit must be positive", ErrInvalid)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open live readers: %w", err)
	}
	db.SetMaxOpenConns(maxConnections)
	db.SetMaxIdleConns(maxConnections)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect live readers: %w", err)
	}
	return db, nil
}
