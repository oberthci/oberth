// Package store owns Oberth's durable SQLite state.
package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/gitoid"
	"github.com/oberthci/oberth/internal/model"

	_ "modernc.org/sqlite"
)

const (
	defaultBusyTimeout = 5 * time.Second
	defaultPageSize    = 50
	maxPageSize        = 100
	IssueLockTTL       = 5 * time.Minute
	sqliteBusyCode     = 5
	sqliteLockedCode   = 6
	sqlitePrimaryMask  = 0xff
	walRetryMaxDelay   = 25 * time.Millisecond
)

var (
	ErrNotFound           = errors.New("store: not found")
	ErrInvalid            = errors.New("store: invalid input")
	ErrInvalidState       = errors.New("store: invalid state transition")
	ErrAmbiguous          = errors.New("store: ambiguous reference")
	ErrLockHeld           = errors.New("store: issue lock held by another identity")
	ErrLockNotOwned       = errors.New("store: issue lock is expired or not owned by identity")
	ErrSchemaTooNew       = errors.New("store: database schema is newer than this binary")
	ErrSchemaIncompatible = errors.New("store: database schema is incompatible with this binary")
	ErrSchemaLegacyV1     = fmt.Errorf("%w: exact predecessor schema version 1", ErrSchemaIncompatible)
)

type Options struct {
	BusyTimeout time.Duration
	Now         func() time.Time
}

type Store struct {
	db       *sql.DB
	now      func() time.Time
	mu       sync.Mutex
	done     bool
	readOnly bool
	cleanup  func() error
}

func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	return open(ctx, path, opts, true, nil)
}

// OpenAdminClient opens the shared current-schema database for a short-lived
// administrative command. It deliberately does not run owner-startup recovery
// because the live server may currently own active Jobs. Existing legacy
// schemas are rejected; only the continuity-bound startup path may migrate v1.
func OpenAdminClient(ctx context.Context, path string, opts Options) (*Store, error) {
	return open(ctx, path, opts, false, nil)
}

// MigrateLegacyV1 upgrades exactly the predecessor v1 schema and returns the
// migrated database without running owner-startup recovery. expectedHead must
// be the head already persisted in immutable rollback-external continuity. The
// same head is checked inside the write transaction before schema v2 commits.
func MigrateLegacyV1(ctx context.Context, path string, expectedHead model.AuditHead, opts Options) (*Store, error) {
	if expectedHead.ID < 0 || len(expectedHead.SHA256) != sha256.Size {
		return nil, fmt.Errorf("%w: legacy migration expected audit head is invalid", ErrInvalid)
	}
	expectedHead.SHA256 = append([]byte(nil), expectedHead.SHA256...)
	return open(ctx, path, opts, false, &expectedHead)
}

// InspectCurrent opens an existing live database read-only and accepts only the
// exact schema understood by this binary. The live daemon uses this handle to
// verify rollback-external audit continuity before opening SQLite writable.
func InspectCurrent(ctx context.Context, path string, opts Options) (*Store, error) {
	snapshotPath, cleanup, err := snapshotCurrentDatabase(path)
	if err != nil {
		return nil, err
	}
	inspection, err := openCurrent(ctx, snapshotPath, opts, true)
	if err != nil {
		return nil, errors.Join(err, cleanup())
	}
	inspection.cleanup = cleanup
	return inspection, nil
}

// InspectLegacyV1 reads and validates the exact predecessor schema from a
// consistent snapshot and returns the audit head that migration v2 will
// deterministically produce. It never opens the live database writable.
func InspectLegacyV1(ctx context.Context, path string, opts Options) (head model.AuditHead, resultErr error) {
	snapshotPath, cleanup, err := snapshotCurrentDatabase(path)
	if err != nil {
		return model.AuditHead{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, cleanup()) }()
	inspection, err := openCurrentUnverified(ctx, snapshotPath, opts, true)
	if err != nil {
		return model.AuditHead{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, inspection.Close()) }()
	version, err := inspection.compatibleSchemaVersion(ctx)
	if err != nil {
		return model.AuditHead{}, err
	}
	if version != 1 {
		return model.AuditHead{}, fmt.Errorf("%w: legacy startup migration requires schema version 1, found %d",
			ErrSchemaIncompatible, version)
	}
	if err := verifyLegacyV1Schema(ctx, inspection.db); err != nil {
		return model.AuditHead{}, err
	}
	return walkLegacyAuditActions(ctx, inspection.db, nil)
}

// OpenCurrent opens an existing live database without changing its journal
// mode, running migrations, or applying owner-startup recovery. The caller must
// first verify the same database through InspectCurrent and the external audit
// continuity protocol.
func OpenCurrent(ctx context.Context, path string, opts Options) (*Store, error) {
	return openCurrent(ctx, path, opts, false)
}

// CreateGenesis exclusively creates and initializes a brand-new database. A
// caller may use this only after proving that rollback-external continuity is
// empty; an existing path is never migrated through this entry point.
func CreateGenesis(ctx context.Context, path string, opts Options) (*Store, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is nil", ErrInvalid)
	}
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w: database path is empty", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- validated operator path.
	if err != nil {
		return nil, fmt.Errorf("create genesis sqlite file: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close genesis sqlite file: %w", err)
	}
	return open(ctx, path, opts, false, nil)
}

func open(ctx context.Context, path string, opts Options, recoverOwnerState bool, expectedLegacyHead *model.AuditHead) (*Store, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is nil", ErrInvalid)
	}
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w: database path is empty", ErrInvalid)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.BusyTimeout <= 0 {
		opts.BusyTimeout = defaultBusyTimeout
	}

	// IMMEDIATE transactions acquire the write reservation before any read.
	// This prevents WAL snapshot-upgrade failures when two Oberth processes
	// concurrently deduplicate a receive event or supersede the same branch.
	dsn, err := immediateTransactionDSN(path, opts.BusyTimeout)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// One connection makes connection-scoped pragmas deterministic. Queue claims
	// still use one atomic UPDATE ... RETURNING statement, so other Oberth
	// processes sharing the file cannot claim the same row.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, now: opts.Now}
	if err := s.configure(ctx, opts.BusyTimeout); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.migrate(ctx, expectedLegacyHead); err != nil {
		_ = db.Close()
		return nil, err
	}
	if recoverOwnerState {
		if err := s.recoverInterruptedRuns(ctx); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return s, nil
}

func openCurrent(ctx context.Context, path string, opts Options, readOnly bool) (*Store, error) {
	s, err := openCurrentUnverified(ctx, path, opts, readOnly)
	if err != nil {
		return nil, err
	}
	if err := s.verifyCurrentSchema(ctx); err != nil {
		_ = s.db.Close()
		return nil, err
	}
	return s, nil
}

func openCurrentUnverified(ctx context.Context, path string, opts Options, readOnly bool) (*Store, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is nil", ErrInvalid)
	}
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w: database path is empty", ErrInvalid)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect current sqlite file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return nil, fmt.Errorf("%w: current sqlite path must be a non-empty regular file", ErrSchemaIncompatible)
	}
	if err := verifySQLiteWALHeader(path); err != nil {
		return nil, err
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.BusyTimeout <= 0 {
		opts.BusyTimeout = defaultBusyTimeout
	}

	mode := "rw"
	if readOnly {
		mode = "ro"
	}
	immutable := false
	if readOnly {
		walInfo, walErr := os.Stat(path + "-wal")
		immutable = errors.Is(walErr, os.ErrNotExist) || (walErr == nil && walInfo.Size() == 0)
		if walErr != nil && !errors.Is(walErr, os.ErrNotExist) {
			return nil, fmt.Errorf("inspect current sqlite WAL: %w", walErr)
		}
	}
	db, err := sql.Open("sqlite", currentDatabaseDSN(path, mode, immutable))
	if err != nil {
		return nil, fmt.Errorf("open current sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, now: opts.Now, readOnly: readOnly}
	if err := s.configureCurrent(ctx, opts.BusyTimeout, readOnly, immutable); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func immediateTransactionDSN(path string, busyTimeout time.Duration) (string, error) {
	base, rawQuery, _ := strings.Cut(path, "?")
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", fmt.Errorf("parse sqlite DSN query: %w", err)
	}
	pragmas := query["_pragma"]
	delete(query, "_pragma")
	for _, pragma := range pragmas {
		if !isBusyTimeoutPragma(pragma) {
			query.Add("_pragma", pragma)
		}
	}
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	query.Set("_txlock", "immediate")
	return base + "?" + query.Encode(), nil
}

func isBusyTimeoutPragma(pragma string) bool {
	pragma = strings.TrimSpace(strings.ToLower(pragma))
	if index := strings.IndexAny(pragma, " \t(="); index >= 0 {
		pragma = pragma[:index]
	}
	return pragma == "busy_timeout"
}

func currentDatabaseDSN(path, mode string, immutable bool) string {
	location := &url.URL{Scheme: "file", Path: path}
	query := location.Query()
	query.Set("mode", mode)
	if immutable {
		query.Set("immutable", "1")
	}
	if mode == "rw" {
		query.Set("_txlock", "immediate")
	}
	location.RawQuery = query.Encode()
	return location.String()
}

type sqliteSnapshotState struct {
	exists bool
	digest [sha256.Size]byte
}

func snapshotCurrentDatabase(path string) (string, func() error, error) {
	directory, err := os.MkdirTemp("/tmp", "oberth-sqlite-inspect-")
	if err != nil {
		return "", nil, fmt.Errorf("create private sqlite inspection directory: %w", err)
	}
	cleanup := func() error {
		if err := os.RemoveAll(directory); err != nil {
			return fmt.Errorf("remove private sqlite inspection directory: %w", err)
		}
		return nil
	}
	destination := filepath.Join(directory, "oberth.sqlite")
	states := make(map[string]sqliteSnapshotState, 2)
	for _, suffix := range []string{"", "-wal"} {
		state, copyErr := copySQLiteSnapshotFile(path+suffix, destination+suffix, suffix == "")
		if copyErr != nil {
			return "", nil, errors.Join(copyErr, cleanup())
		}
		states[suffix] = state
	}
	for _, suffix := range []string{"", "-wal"} {
		current, digestErr := digestSQLiteSourceFile(path+suffix, suffix == "")
		if digestErr != nil {
			return "", nil, errors.Join(digestErr, cleanup())
		}
		if current != states[suffix] {
			return "", nil, errors.Join(errors.New("sqlite source changed while creating read-only startup snapshot"), cleanup())
		}
	}
	return destination, cleanup, nil
}

func copySQLiteSnapshotFile(source, destination string, required bool) (sqliteSnapshotState, error) {
	input, state, err := openSQLiteSourceFile(source, required)
	if err != nil || !state.exists {
		return state, err
	}
	defer func() { _ = input.Close() }()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- private generated path.
	if err != nil {
		return sqliteSnapshotState{}, fmt.Errorf("create private sqlite snapshot file: %w", err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(output, hash), input)
	syncErr := output.Sync()
	closeErr := output.Close()
	if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
		return sqliteSnapshotState{}, fmt.Errorf("copy private sqlite snapshot file: %w", err)
	}
	copy(state.digest[:], hash.Sum(nil))
	return state, nil
}

func digestSQLiteSourceFile(path string, required bool) (sqliteSnapshotState, error) {
	file, state, err := openSQLiteSourceFile(path, required)
	if err != nil || !state.exists {
		return state, err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return sqliteSnapshotState{}, fmt.Errorf("hash sqlite source file: %w", err)
	}
	copy(state.digest[:], hash.Sum(nil))
	return state, nil
}

func openSQLiteSourceFile(path string, required bool) (*os.File, sqliteSnapshotState, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && !required {
		return nil, sqliteSnapshotState{}, nil
	}
	if err != nil {
		return nil, sqliteSnapshotState{}, fmt.Errorf("inspect sqlite source file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, sqliteSnapshotState{}, fmt.Errorf("%w: sqlite source must be a regular file", ErrSchemaIncompatible)
	}
	file, err := os.Open(path) // #nosec G304 -- validated operator path.
	if err != nil {
		return nil, sqliteSnapshotState{}, fmt.Errorf("open sqlite source file: %w", err)
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, sqliteSnapshotState{}, fmt.Errorf("inspect opened sqlite source file: %w", err)
	}
	if !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return nil, sqliteSnapshotState{}, errors.New("sqlite source file changed while opening it")
	}
	return file, sqliteSnapshotState{exists: true}, nil
}

func (s *Store) configure(ctx context.Context, busyTimeout time.Duration) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping sqlite: %w", err)
	}
	for _, statement := range []string{
		`PRAGMA foreign_keys = ON`,
		`PRAGMA synchronous = FULL`,
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure sqlite: %w", err)
		}
	}
	if err := s.enableWAL(ctx, busyTimeout); err != nil {
		return err
	}
	return nil
}

func (s *Store) enableWAL(ctx context.Context, busyTimeout time.Duration) error {
	// journal_mode does not consistently invoke SQLite's busy handler. Disable
	// that connection-level wait for this transition so the Go retry deadline is
	// authoritative, then restore the configured timeout before migrations run.
	if _, err := s.db.ExecContext(ctx, `PRAGMA busy_timeout = 0`); err != nil {
		return fmt.Errorf("disable sqlite WAL busy handler: %w", err)
	}
	var journal string
	if err := retrySQLiteLock(ctx, busyTimeout, func(retryCtx context.Context) error {
		journal = ""
		return s.db.QueryRowContext(retryCtx, `PRAGMA journal_mode = WAL`).Scan(&journal)
	}); err != nil {
		return fmt.Errorf("enable sqlite WAL: %w", err)
	}
	if !strings.EqualFold(journal, "wal") {
		return fmt.Errorf("enable sqlite WAL: journal mode is %q", journal)
	}
	if _, err := s.db.ExecContext(ctx,
		fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("restore sqlite busy timeout after enabling WAL: %w", err)
	}
	return nil
}

// SQLite does not invoke its busy handler for every journal-mode lock conflict.
// Concurrent first openers can therefore receive SQLITE_BUSY immediately even
// after configuring busy_timeout. Retry only the driver's typed lock results,
// and keep the complete retry window bounded by the caller's busy timeout.
func retrySQLiteLock(ctx context.Context, budget time.Duration, operation func(context.Context) error) error {
	retryCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	deadline, _ := retryCtx.Deadline()

	delay := time.Millisecond
	var lastLock error
	for {
		if err := retryCtx.Err(); err != nil {
			return errors.Join(err, lastLock)
		}
		if !time.Now().Before(deadline) {
			return errors.Join(context.DeadlineExceeded, lastLock)
		}
		err := operation(retryCtx)
		if err == nil {
			return nil
		}
		if contextErr := retryCtx.Err(); contextErr != nil {
			return errors.Join(contextErr, err, lastLock)
		}
		if !isSQLiteLockError(err) {
			return err
		}
		lastLock = err
		remaining := time.Until(deadline)
		if remaining <= 0 {
			deadlineErr := retryCtx.Err()
			if deadlineErr == nil {
				deadlineErr = context.DeadlineExceeded
			}
			return errors.Join(deadlineErr, lastLock)
		}
		if delay > remaining {
			delay = remaining
		}
		timer := time.NewTimer(delay)
		select {
		case <-retryCtx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return errors.Join(retryCtx.Err(), lastLock)
		case <-timer.C:
		}
		if delay < walRetryMaxDelay {
			delay *= 2
			if delay > walRetryMaxDelay {
				delay = walRetryMaxDelay
			}
		}
	}
}

func isSQLiteLockError(err error) bool {
	var result interface{ Code() int }
	if !errors.As(err, &result) {
		return false
	}
	switch result.Code() & sqlitePrimaryMask {
	case sqliteBusyCode, sqliteLockedCode:
		return true
	default:
		return false
	}
}

func verifySQLiteWALHeader(path string) error {
	file, err := os.Open(path) // #nosec G304 -- validated operator path.
	if err != nil {
		return fmt.Errorf("read current sqlite header: %w", err)
	}
	defer func() { _ = file.Close() }()
	header := make([]byte, 100)
	if _, err := io.ReadFull(file, header); err != nil {
		return fmt.Errorf("%w: read current sqlite header: %w", ErrSchemaIncompatible, err)
	}
	if string(header[:16]) != "SQLite format 3\x00" || header[18] != 2 || header[19] != 2 {
		return fmt.Errorf("%w: current sqlite header is not a WAL database", ErrSchemaIncompatible)
	}
	return nil
}

func (s *Store) configureCurrent(ctx context.Context, busyTimeout time.Duration, readOnly, immutable bool) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping current sqlite: %w", err)
	}
	statements := []string{
		`PRAGMA foreign_keys = ON`,
		fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeout.Milliseconds()),
	}
	if readOnly {
		statements = append(statements, `PRAGMA query_only = ON`)
	} else {
		statements = append(statements, `PRAGMA synchronous = FULL`)
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure current sqlite: %w", err)
		}
	}
	if !immutable {
		var journal string
		if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil {
			return fmt.Errorf("read current sqlite journal mode: %w", err)
		}
		if !strings.EqualFold(journal, "wal") {
			return fmt.Errorf("%w: current sqlite journal mode is %q, expected WAL", ErrSchemaIncompatible, journal)
		}
	}
	return nil
}

func (s *Store) verifyCurrentSchema(ctx context.Context) error {
	version, err := s.compatibleSchemaVersion(ctx)
	if err != nil {
		return err
	}
	if version == 1 {
		if err := verifyLegacyV1Schema(ctx, s.db); err != nil {
			return err
		}
		return fmt.Errorf("%w: live database requires exact schema version %d", ErrSchemaLegacyV1, latestMigrationVersion)
	}
	if version != latestMigrationVersion {
		var migrationCount, minimumVersion int
		if err := s.db.QueryRowContext(ctx, `
SELECT count(*), COALESCE(MIN(version), 0)
FROM schema_migrations`).Scan(&migrationCount, &minimumVersion); err != nil {
			return fmt.Errorf("%w: read current database schema version: %w", ErrSchemaIncompatible, err)
		}
		return fmt.Errorf("%w: live database requires exact schema version %d; found %d rows spanning %d..%d",
			ErrSchemaIncompatible, latestMigrationVersion, migrationCount, minimumVersion, version)
	}
	return nil
}

func (s *Store) compatibleSchemaVersion(ctx context.Context) (int, error) {
	var migrationCount, minimumVersion, databaseVersion int
	if err := s.db.QueryRowContext(ctx, `
SELECT count(*), COALESCE(MIN(version), 0), COALESCE(MAX(version), 0)
FROM schema_migrations`).Scan(&migrationCount, &minimumVersion, &databaseVersion); err != nil {
		return 0, fmt.Errorf("%w: read current database schema version: %w", ErrSchemaIncompatible, err)
	}
	if databaseVersion > latestMigrationVersion {
		return 0, fmt.Errorf("%w: database=%d binary=%d", ErrSchemaTooNew, databaseVersion, latestMigrationVersion)
	}
	if migrationCount == 0 || minimumVersion != 1 || migrationCount != databaseVersion {
		return 0, fmt.Errorf("%w: migration ledger has %d rows spanning versions %d..%d; expected a contiguous prefix from 1",
			ErrSchemaIncompatible, migrationCount, minimumVersion, databaseVersion)
	}
	if err := verifySchemaIdentity(ctx, s.db); err != nil {
		return 0, err
	}
	return databaseVersion, nil
}

func (s *Store) migrate(ctx context.Context, expectedLegacyHead *model.AuditHead) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, createMigrationLedger); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}
	var migrationCount, minimumVersion, databaseVersion int
	if err := tx.QueryRowContext(ctx, `
SELECT count(*), COALESCE(MIN(version), 0), COALESCE(MAX(version), 0)
FROM schema_migrations`).Scan(&migrationCount, &minimumVersion, &databaseVersion); err != nil {
		return fmt.Errorf("read database schema version: %w", err)
	}
	if databaseVersion > latestMigrationVersion {
		return fmt.Errorf("%w: database=%d binary=%d", ErrSchemaTooNew, databaseVersion, latestMigrationVersion)
	}
	if migrationCount != 0 && (minimumVersion != 1 || migrationCount != databaseVersion) {
		return fmt.Errorf("%w: migration ledger has %d rows spanning versions %d..%d; expected a contiguous prefix from 1",
			ErrSchemaIncompatible, migrationCount, minimumVersion, databaseVersion)
	}
	if databaseVersion == 1 {
		if err := verifyLegacyV1Schema(ctx, tx); err != nil {
			return err
		}
		if expectedLegacyHead == nil {
			return fmt.Errorf("%w: generic database openers cannot migrate existing legacy state", ErrSchemaLegacyV1)
		}
		liveHead, err := walkLegacyAuditActions(ctx, tx, nil)
		if err != nil {
			return err
		}
		if liveHead.ID != expectedLegacyHead.ID || !bytes.Equal(liveHead.SHA256, expectedLegacyHead.SHA256) {
			return fmt.Errorf("%w: live v1 audit head changed after immutable continuity intent", ErrSchemaIncompatible)
		}
	}
	if expectedLegacyHead != nil && databaseVersion != 1 {
		return fmt.Errorf("%w: legacy migration expected schema version %d, found %d",
			ErrSchemaIncompatible, 1, databaseVersion)
	}
	if migrationCount > 0 {
		if err := verifySchemaIdentity(ctx, tx); err != nil {
			return err
		}
	}
	for _, item := range migrations {
		if item.version <= databaseVersion {
			continue
		}
		switch {
		case item.sql != "" && item.apply == nil:
			_, err = tx.ExecContext(ctx, item.sql)
		case item.sql == "" && item.apply != nil:
			err = item.apply(ctx, tx, expectedLegacyHead)
		default:
			err = errors.New("migration must define exactly one application method")
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`, item.version, unixNano(s.now()))
		}
		if err != nil {
			return fmt.Errorf("apply migration %d: %w", item.version, err)
		}
		databaseVersion = item.version
	}
	if err := verifySchemaIdentity(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema migrations: %w", err)
	}
	return nil
}

type schemaIdentityQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func verifySchemaIdentity(ctx context.Context, querier schemaIdentityQuerier) error {
	var identityTables int
	if err := querier.QueryRowContext(ctx, `
SELECT count(*) FROM sqlite_schema
WHERE type = 'table' AND name = 'schema_identity'`).Scan(&identityTables); err != nil {
		return fmt.Errorf("read database schema identity table: %w", err)
	}
	if identityTables != 1 {
		return fmt.Errorf("%w: expected identity %q is missing", ErrSchemaIncompatible, oberthSchemaIdentity)
	}

	var identityRows int
	var identity string
	if err := querier.QueryRowContext(ctx, `
SELECT count(*), COALESCE(MAX(identity), '') FROM schema_identity`).Scan(&identityRows, &identity); err != nil {
		return fmt.Errorf("%w: read identity: %w", ErrSchemaIncompatible, err)
	}
	if identityRows != 1 || identity != oberthSchemaIdentity {
		return fmt.Errorf("%w: identity=%q rows=%d expected=%q",
			ErrSchemaIncompatible, identity, identityRows, oberthSchemaIdentity)
	}
	return nil
}

// RecoverOwnerState applies the live server's restart ownership transition.
// Startup calls it only after the external audit history has been recovered
// and verified; administrative database clients deliberately never call it.
func (s *Store) RecoverOwnerState(ctx context.Context) error {
	return s.recoverInterruptedRuns(ctx)
}

func (s *Store) recoverInterruptedRuns(ctx context.Context) error {
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin owner startup recovery: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO run_cancellations(run_id, job_name, superseded_by, reason, created_at)
SELECT id, job_name, '', 'owner_restart', ?
FROM runs
WHERE status = 'running' AND job_name != ''
  AND NOT EXISTS (
      SELECT 1 FROM publications
      WHERE publications.run_id = runs.id AND publications.status = 'pending'
  )
ON CONFLICT(run_id) DO UPDATE SET
    job_name = excluded.job_name,
    reason = 'owner_restart',
    completed_at = NULL`, now); err != nil {
		return fmt.Errorf("record orphan Job cancellations: %w", err)
	}
	// A supersede can win after ClaimNextRun but before the worker publishes its
	// deterministic Job name. The live worker may still be checking out in that
	// window, so ordinary startup recovery must leave the empty-name obligation
	// pending. Owner startup proves that worker is gone and can terminalize the
	// no-Job cancellation without issuing an unbound Kubernetes deletion.
	if _, err := tx.ExecContext(ctx, `
UPDATE run_cancellations
SET completed_at = ?
WHERE completed_at IS NULL AND job_name = '' AND reason = 'superseded'
  AND EXISTS (
      SELECT 1 FROM runs
      WHERE runs.id = run_cancellations.run_id
        AND runs.status = 'interrupted' AND runs.superseded_by != ''
  )`, now); err != nil {
		return fmt.Errorf("complete owner-restart no-Job cancellations: %w", err)
	}
	runRows, err := tx.QueryContext(ctx, `
SELECT `+runColumns+` FROM runs
WHERE status = 'running'
  AND NOT EXISTS (
      SELECT 1 FROM publications
      WHERE publications.run_id = runs.id AND publications.status = 'pending'
  )
ORDER BY queue_sequence`)
	if err != nil {
		return fmt.Errorf("load active runs for recovery: %w", err)
	}
	defer func() { _ = runRows.Close() }()
	var activeRuns []model.Run
	for runRows.Next() {
		run, scanErr := scanRun(runRows)
		if scanErr != nil {
			return fmt.Errorf("scan active recovery run: %w", scanErr)
		}
		activeRuns = append(activeRuns, run)
	}
	if err := runRows.Err(); err != nil {
		return fmt.Errorf("list active recovery runs: %w", err)
	}
	for _, active := range activeRuns {
		if active.JobName != "" {
			// Runs with a known Job name stay in "running" status so the
			// scheduler's startup reconciliation can query K8s for their
			// terminal state before deciding whether to interrupt.
			continue
		}
		recovered, updateErr := scanRun(tx.QueryRowContext(ctx, `
UPDATE runs
SET status = 'interrupted', phase = 'interrupted', reason = 'oberth restarted',
    error = CASE WHEN error = '' THEN 'oberth restarted' ELSE error END,
    finished_at = ?, updated_at = ?
WHERE id = ? AND status = 'running'
RETURNING `+runColumns, now, now, active.ID))
		if updateErr != nil {
			return fmt.Errorf("recover active run %s: %w", active.ID, updateErr)
		}
		if recovered.Trigger == "promotion" {
			promotion, promotionErr := promotionByRunTx(ctx, tx, recovered.ID)
			if promotionErr != nil {
				return fmt.Errorf("load interrupted promotion for run %s: %w", recovered.ID, promotionErr)
			}
			promotion, promotionErr = finishPromotionTx(
				ctx, tx, promotion, model.PromotionInterrupted, recovered.ID,
				"oberth restarted during promotion CI", now,
			)
			if promotionErr != nil {
				return promotionErr
			}
			if err := projectPromotionIssue(ctx, tx, promotion, now); err != nil {
				return err
			}
		} else if err := projectRunIssue(ctx, tx, recovered, "", now); err != nil {
			return err
		}
	}

	promotionRows, err := tx.QueryContext(ctx, `
SELECT sequence, id, repo_id, source_branch, source_sha, target_ref, previous_sha,
       result_sha, actor, status, run_id, error, created_at, updated_at
FROM promotions
WHERE status = 'pending'
  AND (
      run_id = ''
      OR EXISTS (
          SELECT 1 FROM runs
          WHERE runs.id = promotions.run_id
            AND runs.status IN ('passed', 'failed', 'interrupted')
      )
  )
  AND NOT EXISTS (
      SELECT 1 FROM publications
      WHERE publications.promotion_id = promotions.id
        AND publications.status = 'pending'
  )
ORDER BY sequence`)
	if err != nil {
		return fmt.Errorf("load pending promotions for recovery: %w", err)
	}
	defer func() { _ = promotionRows.Close() }()
	var pendingPromotions []model.Promotion
	for promotionRows.Next() {
		promotion, scanErr := scanPromotion(promotionRows)
		if scanErr != nil {
			return fmt.Errorf("scan pending recovery promotion: %w", scanErr)
		}
		pendingPromotions = append(pendingPromotions, promotion)
	}
	if err := promotionRows.Err(); err != nil {
		return fmt.Errorf("list pending recovery promotions: %w", err)
	}
	for _, pending := range pendingPromotions {
		status := model.PromotionInterrupted
		failure := "oberth restarted before promotion publication intent"
		if pending.RunID != "" {
			run, runErr := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, pending.RunID))
			if runErr != nil {
				return fmt.Errorf("load terminal promotion run %s: %w", pending.RunID, runErr)
			}
			if run.Status == model.RunInterrupted {
				failure = "oberth restarted during promotion CI"
			} else {
				status = model.PromotionFailed
				failure = run.Error
				if strings.TrimSpace(failure) == "" {
					failure = "terminal promotion run has no publication intent"
				}
			}
		}
		recovered, recoveryErr := finishPromotionTx(ctx, tx, pending, status, pending.RunID, failure, now)
		if recoveryErr != nil {
			return recoveryErr
		}
		if err := projectPromotionIssue(ctx, tx, recovered, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit owner startup recovery: %w", err)
	}
	return nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return nil
	}
	s.done = true
	if s.readOnly {
		closeErr := s.db.Close()
		if s.cleanup != nil {
			return errors.Join(closeErr, s.cleanup())
		}
		return closeErr
	}
	if _, err := s.db.ExecContext(context.Background(), `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		_ = s.db.Close()
		return fmt.Errorf("checkpoint sqlite WAL: %w", err)
	}
	return s.db.Close()
}

func (s *Store) CreateUpstream(ctx context.Context, spec model.UpstreamSpec) (model.Upstream, error) {
	if strings.TrimSpace(spec.Name) == "" || strings.TrimSpace(spec.Kind) == "" || strings.TrimSpace(spec.BaseURL) == "" {
		return model.Upstream{}, fmt.Errorf("%w: upstream fields are required", ErrInvalid)
	}
	now := unixNano(s.now())
	result, err := s.db.ExecContext(ctx, `
INSERT INTO upstreams(name, kind, base_url, created_at, updated_at) VALUES(?, ?, ?, ?, ?)`,
		spec.Name, spec.Kind, spec.BaseURL, now, now)
	if err != nil {
		return model.Upstream{}, fmt.Errorf("create upstream: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return model.Upstream{}, fmt.Errorf("read upstream id: %w", err)
	}
	return s.Upstream(ctx, id)
}

func (s *Store) Upstream(ctx context.Context, id int64) (model.Upstream, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, name, kind, base_url, created_at, updated_at FROM upstreams WHERE id = ?`, id)
	var value model.Upstream
	var created, updated int64
	if err := row.Scan(&value.ID, &value.Name, &value.Kind, &value.BaseURL, &created, &updated); err != nil {
		return model.Upstream{}, translateNotFound("upstream", err)
	}
	value.CreatedAt, value.UpdatedAt = fromUnixNano(created), fromUnixNano(updated)
	return value, nil
}

func (s *Store) CreateRepository(ctx context.Context, spec model.RepositorySpec) (model.Repository, error) {
	if strings.TrimSpace(spec.Name) == "" || spec.UpstreamID <= 0 || strings.TrimSpace(spec.DefaultBranch) == "" {
		return model.Repository{}, fmt.Errorf("%w: repository fields are required", ErrInvalid)
	}
	now := unixNano(s.now())
	result, err := s.db.ExecContext(ctx, `
INSERT INTO repositories(name, upstream_id, default_branch, created_at, updated_at) VALUES(?, ?, ?, ?, ?)`,
		spec.Name, spec.UpstreamID, spec.DefaultBranch, now, now)
	if err != nil {
		return model.Repository{}, fmt.Errorf("create repository: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return model.Repository{}, fmt.Errorf("read repository id: %w", err)
	}
	return s.Repository(ctx, id)
}

func (s *Store) Repository(ctx context.Context, id int64) (model.Repository, error) {
	return scanRepository(s.db.QueryRowContext(ctx, `
SELECT id, name, upstream_id, default_branch, created_at, updated_at FROM repositories WHERE id = ?`, id))
}

func (s *Store) RepositoryByName(ctx context.Context, name string) (model.Repository, error) {
	return scanRepository(s.db.QueryRowContext(ctx, `
SELECT id, name, upstream_id, default_branch, created_at, updated_at FROM repositories WHERE name = ?`, name))
}

func (s *Store) SetRepositoryDefaultBranch(ctx context.Context, id int64, branch string) (model.Repository, error) {
	if id <= 0 || strings.TrimSpace(branch) == "" {
		return model.Repository{}, fmt.Errorf("%w: repository and default branch are required", ErrInvalid)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE repositories SET default_branch = ?, updated_at = ? WHERE id = ?`, branch, unixNano(s.now()), id)
	if err != nil {
		return model.Repository{}, fmt.Errorf("update repository default branch: %w", err)
	}
	if err := requireChanged("repository", result); err != nil {
		return model.Repository{}, err
	}
	return s.Repository(ctx, id)
}

func (s *Store) ListRepositories(ctx context.Context) ([]model.Repository, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, name, upstream_id, default_branch, created_at, updated_at
FROM repositories ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list repositories: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var repositories []model.Repository
	for rows.Next() {
		var value model.Repository
		var created, updated int64
		if err := rows.Scan(&value.ID, &value.Name, &value.UpstreamID, &value.DefaultBranch, &created, &updated); err != nil {
			return nil, fmt.Errorf("scan repository: %w", err)
		}
		value.CreatedAt, value.UpdatedAt = fromUnixNano(created), fromUnixNano(updated)
		repositories = append(repositories, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list repositories: %w", err)
	}
	return repositories, nil
}

func scanRepository(row *sql.Row) (model.Repository, error) {
	var value model.Repository
	var created, updated int64
	if err := row.Scan(&value.ID, &value.Name, &value.UpstreamID, &value.DefaultBranch, &created, &updated); err != nil {
		return model.Repository{}, translateNotFound("repository", err)
	}
	value.CreatedAt, value.UpdatedAt = fromUnixNano(created), fromUnixNano(updated)
	return value, nil
}

func (s *Store) AppendPromotion(ctx context.Context, spec model.PromotionSpec) (model.Promotion, error) {
	return s.appendPromotion(ctx, spec, "")
}

// AppendPromotionForRun atomically records the immutable relationship between
// a divergent promotion and the merged-tree CI run created for it.
func (s *Store) AppendPromotionForRun(ctx context.Context, spec model.PromotionSpec, runID string) (model.Promotion, error) {
	if strings.TrimSpace(runID) == "" {
		return model.Promotion{}, fmt.Errorf("%w: promotion run ID is required", ErrInvalid)
	}
	return s.appendPromotion(ctx, spec, runID)
}

func (s *Store) appendPromotion(ctx context.Context, spec model.PromotionSpec, runID string) (model.Promotion, error) {
	if err := validatePromotionAdmissionSpec(spec); err != nil {
		return model.Promotion{}, err
	}
	id, err := randomID()
	if err != nil {
		return model.Promotion{}, fmt.Errorf("generate promotion id: %w", err)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Promotion{}, fmt.Errorf("begin append promotion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	value, err := insertPromotion(ctx, tx, spec, id, runID, now)
	if err != nil {
		return model.Promotion{}, err
	}
	if err := registerPromotionIssueWork(ctx, tx, value, now); err != nil {
		return model.Promotion{}, err
	}
	if err := appendPromotionAdmissionAudit(ctx, tx, value, now); err != nil {
		return model.Promotion{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Promotion{}, fmt.Errorf("commit append promotion: %w", err)
	}
	return value, nil
}

// EnqueuePromotionRun commits the merged-tree run and its immutable promotion
// association together. The scheduler can therefore never observe one without
// the other, even when another enqueue wakes it concurrently.
func (s *Store) EnqueuePromotionRun(ctx context.Context, runSpec model.RunSpec, promotionSpec model.PromotionSpec) (model.EnqueueRunResult, model.Promotion, error) {
	validatedRun, err := validateRunSpec(runSpec)
	if err != nil {
		return model.EnqueueRunResult{}, model.Promotion{}, err
	}
	if err := validatePromotionSpec(promotionSpec); err != nil {
		return model.EnqueueRunResult{}, model.Promotion{}, err
	}
	if validatedRun.Trigger != "promotion" || validatedRun.Release || validatedRun.RefKind != model.RefBranch ||
		validatedRun.RepoID != promotionSpec.RepoID || validatedRun.Actor != promotionSpec.Actor ||
		!strings.EqualFold(validatedRun.SHA, promotionSpec.ResultSHA) ||
		!strings.EqualFold(validatedRun.TestedSHA, promotionSpec.ResultSHA) ||
		!strings.EqualFold(validatedRun.BaseSHA, promotionSpec.PreviousSHA) {
		return model.EnqueueRunResult{}, model.Promotion{}, fmt.Errorf("%w: promotion run does not match promotion", ErrInvalid)
	}
	runID, err := randomID()
	if err != nil {
		return model.EnqueueRunResult{}, model.Promotion{}, fmt.Errorf("generate promotion run id: %w", err)
	}
	promotionID, err := randomID()
	if err != nil {
		return model.EnqueueRunResult{}, model.Promotion{}, fmt.Errorf("generate promotion id: %w", err)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.EnqueueRunResult{}, model.Promotion{}, fmt.Errorf("begin promotion run enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	enqueued, err := s.enqueueRunTx(ctx, tx, validatedRun, runID, now)
	if err != nil {
		return model.EnqueueRunResult{}, model.Promotion{}, err
	}
	promotion, err := insertPromotion(ctx, tx, promotionSpec, promotionID, runID, now)
	if err != nil {
		return model.EnqueueRunResult{}, model.Promotion{}, err
	}
	if err := registerPromotionIssueWork(ctx, tx, promotion, now); err != nil {
		return model.EnqueueRunResult{}, model.Promotion{}, err
	}
	if err := appendPromotionAdmissionAudit(ctx, tx, promotion, now); err != nil {
		return model.EnqueueRunResult{}, model.Promotion{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.EnqueueRunResult{}, model.Promotion{}, fmt.Errorf("commit promotion run enqueue: %w", err)
	}
	return enqueued, promotion, nil
}

// PlanPromotion fills the immutable Git plan for an already admitted
// promotion. Repeating the exact plan is idempotent; changing it fails closed.
func (s *Store) PlanPromotion(ctx context.Context, id, previousSHA, resultSHA string) (model.Promotion, error) {
	if strings.TrimSpace(id) == "" || !validOID(resultSHA) || (previousSHA != "" && !validOID(previousSHA)) {
		return model.Promotion{}, fmt.Errorf("%w: promotion plan fields are invalid", ErrInvalid)
	}
	now := unixNano(s.now())
	value, err := scanPromotion(s.db.QueryRowContext(ctx, `
UPDATE promotions
SET previous_sha = ?, result_sha = ?, updated_at = ?
WHERE id = ? AND status = 'pending' AND previous_sha = '' AND result_sha = ''
RETURNING sequence, id, repo_id, source_branch, source_sha, target_ref, previous_sha,
          result_sha, actor, status, run_id, error, created_at, updated_at`,
		strings.ToLower(previousSHA), strings.ToLower(resultSHA), now, id))
	if err == nil {
		return value, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return model.Promotion{}, fmt.Errorf("plan promotion: %w", err)
	}
	current, lookupErr := s.Promotion(ctx, id)
	if lookupErr != nil {
		return model.Promotion{}, lookupErr
	}
	if current.Status == model.PromotionPending && sameStoredOID(current.PreviousSHA, previousSHA) && sameStoredOID(current.ResultSHA, resultSHA) {
		return current, nil
	}
	return model.Promotion{}, fmt.Errorf("%w: promotion plan is already fixed", ErrInvalidState)
}

// EnqueueAdmittedPromotionRun commits a merged-tree run and attaches it to an
// existing planned promotion in one transaction.
func (s *Store) EnqueueAdmittedPromotionRun(ctx context.Context, runSpec model.RunSpec, promotionID string) (model.EnqueueRunResult, model.Promotion, error) {
	validatedRun, err := validateRunSpec(runSpec)
	if err != nil {
		return model.EnqueueRunResult{}, model.Promotion{}, err
	}
	if strings.TrimSpace(promotionID) == "" {
		return model.EnqueueRunResult{}, model.Promotion{}, fmt.Errorf("%w: promotion ID is required", ErrInvalid)
	}
	runID, err := randomID()
	if err != nil {
		return model.EnqueueRunResult{}, model.Promotion{}, fmt.Errorf("generate promotion run id: %w", err)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.EnqueueRunResult{}, model.Promotion{}, fmt.Errorf("begin admitted promotion run enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	promotion, err := promotionTx(ctx, tx, promotionID)
	if err != nil {
		return model.EnqueueRunResult{}, model.Promotion{}, err
	}
	if promotion.Status != model.PromotionPending || promotion.RunID != "" ||
		!validOID(promotion.ResultSHA) || validatedRun.Trigger != "promotion" || validatedRun.Release ||
		validatedRun.RefKind != model.RefBranch || validatedRun.RepoID != promotion.RepoID ||
		validatedRun.Actor != promotion.Actor || !strings.EqualFold(validatedRun.SHA, promotion.ResultSHA) ||
		!strings.EqualFold(validatedRun.TestedSHA, promotion.ResultSHA) ||
		!strings.EqualFold(validatedRun.BaseSHA, promotion.PreviousSHA) {
		return model.EnqueueRunResult{}, model.Promotion{}, fmt.Errorf("%w: admitted promotion run does not match promotion", ErrInvalidState)
	}
	enqueued, err := s.enqueueRunTx(ctx, tx, validatedRun, runID, now)
	if err != nil {
		return model.EnqueueRunResult{}, model.Promotion{}, err
	}
	promotion, err = scanPromotion(tx.QueryRowContext(ctx, `
UPDATE promotions SET run_id = ?, updated_at = ?
WHERE id = ? AND status = 'pending' AND run_id = ''
RETURNING sequence, id, repo_id, source_branch, source_sha, target_ref, previous_sha,
          result_sha, actor, status, run_id, error, created_at, updated_at`, runID, now, promotionID))
	if err != nil {
		return model.EnqueueRunResult{}, model.Promotion{}, fmt.Errorf("attach admitted promotion run: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.EnqueueRunResult{}, model.Promotion{}, fmt.Errorf("commit admitted promotion run enqueue: %w", err)
	}
	return enqueued, promotion, nil
}

type promotionQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func insertPromotion(ctx context.Context, queryer promotionQueryer, spec model.PromotionSpec, id, runID string, now int64) (model.Promotion, error) {
	row := queryer.QueryRowContext(ctx, `
INSERT INTO promotions(id, repo_id, source_branch, source_sha, target_ref, previous_sha, result_sha, actor, status, run_id, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?)
RETURNING sequence, id, repo_id, source_branch, source_sha, target_ref, previous_sha,
          result_sha, actor, status, run_id, error, created_at, updated_at`,
		id, spec.RepoID, spec.SourceBranch, spec.SourceSHA, spec.TargetRef,
		spec.PreviousSHA, spec.ResultSHA, spec.Actor, runID, now, now)
	value, err := scanPromotion(row)
	if err != nil {
		return model.Promotion{}, fmt.Errorf("append promotion: %w", err)
	}
	return value, nil
}

func validatePromotionSpec(spec model.PromotionSpec) error {
	if err := validatePromotionAdmissionSpec(spec); err != nil {
		return err
	}
	if !validOID(spec.ResultSHA) {
		return fmt.Errorf("%w: promotion result SHA is invalid", ErrInvalid)
	}
	return nil
}

func validatePromotionAdmissionSpec(spec model.PromotionSpec) error {
	if spec.RepoID <= 0 || strings.TrimSpace(spec.SourceBranch) == "" || !validOID(spec.SourceSHA) ||
		strings.TrimSpace(spec.TargetRef) == "" || strings.TrimSpace(spec.Actor) == "" {
		return fmt.Errorf("%w: promotion fields are invalid", ErrInvalid)
	}
	if (spec.PreviousSHA == "") != (spec.ResultSHA == "") {
		return fmt.Errorf("%w: promotion plan must be wholly present or absent", ErrInvalid)
	}
	if spec.PreviousSHA != "" && !validOID(spec.PreviousSHA) {
		return fmt.Errorf("%w: previous promotion SHA is invalid", ErrInvalid)
	}
	if spec.ResultSHA != "" && !validOID(spec.ResultSHA) {
		return fmt.Errorf("%w: promotion result SHA is invalid", ErrInvalid)
	}
	return nil
}

func appendPromotionAdmissionAudit(ctx context.Context, tx *sql.Tx, promotion model.Promotion, now int64) error {
	details, err := json.Marshal(map[string]string{
		"source_branch": promotion.SourceBranch,
		"source_sha":    promotion.SourceSHA,
		"target_ref":    promotion.TargetRef,
	})
	if err != nil {
		return fmt.Errorf("encode promotion admission audit: %w", err)
	}
	if _, err := appendAuditAction(ctx, tx, model.AuditActionSpec{
		Actor: promotion.Actor, Action: "promotion.admit", ResourceType: "promotion",
		ResourceID: promotion.ID, Details: string(details),
	}, now); err != nil {
		return fmt.Errorf("append promotion admission audit: %w", err)
	}
	return nil
}

func (s *Store) PromotionByRun(ctx context.Context, runID string) (model.Promotion, error) {
	if strings.TrimSpace(runID) == "" {
		return model.Promotion{}, fmt.Errorf("%w: promotion run ID is required", ErrInvalid)
	}
	var matches int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM promotions WHERE run_id = ?`, runID).Scan(&matches); err != nil {
		return model.Promotion{}, fmt.Errorf("count promotion run associations: %w", err)
	}
	if matches == 0 {
		return model.Promotion{}, fmt.Errorf("%w: promotion run", ErrNotFound)
	}
	if matches > 1 {
		return model.Promotion{}, fmt.Errorf("%w: promotion run %s", ErrAmbiguous, runID)
	}
	value, err := scanPromotion(s.db.QueryRowContext(ctx, `
SELECT sequence, id, repo_id, source_branch, source_sha, target_ref, previous_sha,
       result_sha, actor, status, run_id, error, created_at, updated_at
FROM promotions WHERE run_id = ?`, runID))
	if err != nil {
		return model.Promotion{}, translateNotFound("promotion run", err)
	}
	return value, nil
}

func promotionByRunTx(ctx context.Context, tx *sql.Tx, runID string) (model.Promotion, error) {
	value, err := scanPromotion(tx.QueryRowContext(ctx, `
SELECT sequence, id, repo_id, source_branch, source_sha, target_ref, previous_sha,
       result_sha, actor, status, run_id, error, created_at, updated_at
FROM promotions WHERE run_id = ?`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Promotion{}, fmt.Errorf("%w: promotion run", ErrNotFound)
	}
	if err != nil {
		return model.Promotion{}, fmt.Errorf("load promotion run: %w", err)
	}
	return value, nil
}

func (s *Store) Promotion(ctx context.Context, id string) (model.Promotion, error) {
	value, err := scanPromotion(s.db.QueryRowContext(ctx, `
SELECT sequence, id, repo_id, source_branch, source_sha, target_ref, previous_sha,
       result_sha, actor, status, run_id, error, created_at, updated_at
FROM promotions WHERE id = ?`, id))
	if err != nil {
		return model.Promotion{}, translateNotFound("promotion", err)
	}
	return value, nil
}

func promotionTx(ctx context.Context, tx *sql.Tx, id string) (model.Promotion, error) {
	value, err := scanPromotion(tx.QueryRowContext(ctx, `
SELECT sequence, id, repo_id, source_branch, source_sha, target_ref, previous_sha,
       result_sha, actor, status, run_id, error, created_at, updated_at
FROM promotions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Promotion{}, fmt.Errorf("%w: promotion", ErrNotFound)
	}
	if err != nil {
		return model.Promotion{}, fmt.Errorf("load promotion: %w", err)
	}
	return value, nil
}

func (s *Store) FinishPromotion(ctx context.Context, id string, status model.PromotionStatus, runID, failure string) (model.Promotion, error) {
	if strings.TrimSpace(id) == "" || (status != model.PromotionFailed && status != model.PromotionInterrupted) {
		return model.Promotion{}, fmt.Errorf("%w: promotion failures finish directly; success finalizes through publication", ErrInvalid)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Promotion{}, fmt.Errorf("begin finish promotion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	current, err := promotionTx(ctx, tx, id)
	if err != nil {
		return model.Promotion{}, err
	}
	value, err := finishPromotionTx(ctx, tx, current, status, runID, failure, now)
	if err != nil {
		return model.Promotion{}, err
	}
	if err := projectPromotionIssue(ctx, tx, value, now); err != nil {
		return model.Promotion{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Promotion{}, fmt.Errorf("commit finish promotion: %w", err)
	}
	return value, nil
}

func finishPromotionTx(ctx context.Context, tx *sql.Tx, current model.Promotion, status model.PromotionStatus, runID, failure string, now int64) (model.Promotion, error) {
	value, err := scanPromotion(tx.QueryRowContext(ctx, `
UPDATE promotions
SET status = ?, run_id = ?, error = ?, updated_at = ?
WHERE id = ? AND status = 'pending'
RETURNING sequence, id, repo_id, source_branch, source_sha, target_ref, previous_sha,
          result_sha, actor, status, run_id, error, created_at, updated_at`,
		status, runID, failure, now, current.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Promotion{}, fmt.Errorf("%w: promotion is already terminal", ErrInvalidState)
	}
	if err != nil {
		return model.Promotion{}, fmt.Errorf("finish promotion: %w", err)
	}
	return value, nil
}

func (s *Store) ListPromotions(ctx context.Context, repoID int64, limit int) ([]model.Promotion, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT sequence, id, repo_id, source_branch, source_sha, target_ref, previous_sha,
       result_sha, actor, status, run_id, error, created_at, updated_at
FROM promotions WHERE (? = 0 OR repo_id = ?) ORDER BY sequence DESC LIMIT ?`,
		repoID, repoID, pageLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("list promotions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var promotions []model.Promotion
	for rows.Next() {
		value, err := scanPromotion(rows)
		if err != nil {
			return nil, fmt.Errorf("scan promotion: %w", err)
		}
		promotions = append(promotions, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list promotions: %w", err)
	}
	return promotions, nil
}

func scanPromotion(row rowScanner) (model.Promotion, error) {
	var value model.Promotion
	var created, updated int64
	if err := row.Scan(&value.Sequence, &value.ID, &value.RepoID, &value.SourceBranch,
		&value.SourceSHA, &value.TargetRef, &value.PreviousSHA, &value.ResultSHA,
		&value.Actor, &value.Status, &value.RunID, &value.Error, &created, &updated); err != nil {
		return model.Promotion{}, err
	}
	value.CreatedAt, value.UpdatedAt = fromUnixNano(created), fromUnixNano(updated)
	return value, nil
}

func (s *Store) UpsertUplink(ctx context.Context, spec model.UplinkSpec) (model.Uplink, error) {
	if strings.TrimSpace(spec.Fingerprint) == "" || strings.TrimSpace(spec.Identity) == "" ||
		strings.TrimSpace(spec.TokenCredentialID) == "" || strings.TrimSpace(spec.AuthActor) == "" {
		return model.Uplink{}, fmt.Errorf("%w: uplink fields are required", ErrInvalid)
	}
	now := unixNano(s.now())
	row := s.db.QueryRowContext(ctx, `
INSERT INTO uplinks(fingerprint, identity, token_credential_id, auth_actor, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?)
ON CONFLICT(fingerprint) DO UPDATE SET
    identity = excluded.identity,
    token_credential_id = excluded.token_credential_id,
    auth_actor = excluded.auth_actor,
    updated_at = excluded.updated_at
RETURNING id, fingerprint, identity, token_credential_id, auth_actor, created_at, updated_at`,
		spec.Fingerprint, spec.Identity, spec.TokenCredentialID, spec.AuthActor, now, now)
	var value model.Uplink
	var created, updated int64
	if err := row.Scan(&value.ID, &value.Fingerprint, &value.Identity, &value.TokenCredentialID, &value.AuthActor, &created, &updated); err != nil {
		return model.Uplink{}, fmt.Errorf("upsert uplink: %w", err)
	}
	value.CreatedAt, value.UpdatedAt = fromUnixNano(created), fromUnixNano(updated)
	return value, nil
}

func (s *Store) UplinkByFingerprint(ctx context.Context, fingerprint string) (model.Uplink, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, fingerprint, identity, token_credential_id, auth_actor, created_at, updated_at
FROM uplinks WHERE fingerprint = ?`, fingerprint)
	var value model.Uplink
	var created, updated int64
	if err := row.Scan(&value.ID, &value.Fingerprint, &value.Identity, &value.TokenCredentialID, &value.AuthActor, &created, &updated); err != nil {
		return model.Uplink{}, translateNotFound("uplink", err)
	}
	value.CreatedAt, value.UpdatedAt = fromUnixNano(created), fromUnixNano(updated)
	return value, nil
}

func (s *Store) AppendAuditAction(ctx context.Context, spec model.AuditActionSpec) (model.AuditAction, error) {
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.AuditAction{}, fmt.Errorf("begin audit action: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	value, err := appendAuditAction(ctx, tx, spec, now)
	if err != nil {
		return model.AuditAction{}, fmt.Errorf("append audit action: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.AuditAction{}, fmt.Errorf("commit audit action: %w", err)
	}
	return value, nil
}

func (s *Store) CreateTokenCredential(ctx context.Context, spec model.TokenCredentialSpec) (model.TokenCredential, error) {
	if strings.TrimSpace(spec.Name) == "" || len(spec.Digest) != 32 {
		return model.TokenCredential{}, fmt.Errorf("%w: token name and SHA-256 digest are required", ErrInvalid)
	}
	id, err := randomID()
	if err != nil {
		return model.TokenCredential{}, fmt.Errorf("generate token credential id: %w", err)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.TokenCredential{}, fmt.Errorf("begin create token credential: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO token_credentials(id, name, digest, created_at, revoked_at) VALUES(?, ?, ?, ?, ?)`,
		id, spec.Name, append([]byte(nil), spec.Digest...), now, now); err != nil {
		return model.TokenCredential{}, fmt.Errorf("create token credential: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO pending_token_credentials(token_credential_id, created_at) VALUES(?, ?)`, id, now); err != nil {
		return model.TokenCredential{}, fmt.Errorf("mark token credential pending: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.TokenCredential{}, fmt.Errorf("commit token credential: %w", err)
	}
	pendingAt := fromUnixNano(now)
	return model.TokenCredential{ID: id, Name: spec.Name, Digest: append([]byte(nil), spec.Digest...), CreatedAt: pendingAt, RevokedAt: &pendingAt}, nil
}

func (s *Store) ActivateTokenCredential(ctx context.Context, id string) (model.TokenCredential, error) {
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.TokenCredential{}, fmt.Errorf("begin activate token credential: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var pendingID string
	if err := tx.QueryRowContext(ctx, `
DELETE FROM pending_token_credentials WHERE token_credential_id = ? RETURNING token_credential_id`, id).Scan(&pendingID); err != nil {
		return model.TokenCredential{}, translateNotFound("pending token credential", err)
	}
	value, err := scanTokenCredential(tx.QueryRowContext(ctx, `
UPDATE token_credentials SET activated_at = ?, revoked_at = NULL
WHERE id = ? AND activated_at IS NULL
  AND EXISTS (SELECT 1 FROM uplinks WHERE token_credential_id = token_credentials.id)
RETURNING id, name, digest, created_at, activated_at, last_used_at, revoked_at`, now, id))
	if err != nil {
		return model.TokenCredential{}, translateNotFound("pending token credential", err)
	}
	if err := tx.Commit(); err != nil {
		return model.TokenCredential{}, fmt.Errorf("commit activate token credential: %w", err)
	}
	return value, nil
}

func (s *Store) TokenCredentialByDigest(ctx context.Context, digest []byte) (model.TokenCredential, error) {
	if len(digest) != 32 {
		return model.TokenCredential{}, ErrNotFound
	}
	value, err := scanTokenCredential(s.db.QueryRowContext(ctx, `
SELECT id, name, digest, created_at, activated_at, last_used_at, revoked_at
FROM token_credentials WHERE digest = ?`, digest))
	if err != nil {
		return model.TokenCredential{}, translateNotFound("token credential", err)
	}
	return value, nil
}

func (s *Store) AuthenticatedUplinkByDigest(ctx context.Context, digest []byte) (model.AuthenticatedUplink, error) {
	if len(digest) != 32 {
		return model.AuthenticatedUplink{}, ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, `
SELECT u.id, u.fingerprint, u.identity, u.token_credential_id, u.auth_actor, u.created_at, u.updated_at,
       t.id, t.name, t.digest, t.created_at, t.activated_at, t.last_used_at, t.revoked_at
FROM token_credentials t
JOIN uplinks u ON u.token_credential_id = t.id
WHERE t.digest = ? AND t.activated_at IS NOT NULL AND t.revoked_at IS NULL`, digest)
	var value model.AuthenticatedUplink
	var uplinkCreated, uplinkUpdated, tokenCreated int64
	var activated, lastUsed, revoked sql.NullInt64
	if err := row.Scan(
		&value.ID, &value.Fingerprint, &value.Identity, &value.TokenCredentialID,
		&value.AuthActor, &uplinkCreated, &uplinkUpdated,
		&value.TokenCredential.ID, &value.TokenCredential.Name, &value.TokenCredential.Digest,
		&tokenCreated, &activated, &lastUsed, &revoked,
	); err != nil {
		return model.AuthenticatedUplink{}, translateNotFound("authenticated uplink", err)
	}
	value.CreatedAt, value.UpdatedAt = fromUnixNano(uplinkCreated), fromUnixNano(uplinkUpdated)
	value.TokenCredential.CreatedAt = fromUnixNano(tokenCreated)
	value.TokenCredential.ActivatedAt = nullableTime(activated)
	value.TokenCredential.LastUsedAt = nullableTime(lastUsed)
	value.TokenCredential.RevokedAt = nullableTime(revoked)
	return value, nil
}

func scanTokenCredential(row rowScanner) (model.TokenCredential, error) {
	var value model.TokenCredential
	var created int64
	var activated, lastUsed, revoked sql.NullInt64
	if err := row.Scan(&value.ID, &value.Name, &value.Digest, &created, &activated, &lastUsed, &revoked); err != nil {
		return model.TokenCredential{}, err
	}
	value.CreatedAt = fromUnixNano(created)
	value.ActivatedAt = nullableTime(activated)
	value.LastUsedAt = nullableTime(lastUsed)
	value.RevokedAt = nullableTime(revoked)
	return value, nil
}

func (s *Store) TouchTokenCredential(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `
UPDATE token_credentials SET last_used_at = ?
WHERE id = ? AND activated_at IS NOT NULL AND revoked_at IS NULL`, unixNano(s.now()), id)
	if err != nil {
		return fmt.Errorf("touch token credential: %w", err)
	}
	return requireChanged("token credential", result)
}

func (s *Store) RevokeTokenCredential(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin revoke token credential: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	pendingResult, err := tx.ExecContext(ctx, `DELETE FROM pending_token_credentials WHERE token_credential_id = ?`, id)
	if err != nil {
		return fmt.Errorf("clear pending token credential: %w", err)
	}
	pending, err := pendingResult.RowsAffected()
	if err != nil {
		return fmt.Errorf("read pending token credential change: %w", err)
	}
	query := `UPDATE token_credentials SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`
	if pending > 0 {
		query = `UPDATE token_credentials SET revoked_at = ? WHERE id = ?`
	}
	result, err := tx.ExecContext(ctx, query, unixNano(s.now()), id)
	if err != nil {
		return fmt.Errorf("revoke token credential: %w", err)
	}
	if err := requireChanged("token credential", result); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit revoke token credential: %w", err)
	}
	return nil
}

func randomID() (string, error) {
	var body [16]byte
	if _, err := rand.Read(body[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(body[:]), nil
}

func validOID(value string) bool { return gitoid.Valid(value) }

func unixNano(value time.Time) int64 { return value.UTC().UnixNano() }

func fromUnixNano(value int64) time.Time { return time.Unix(0, value).UTC() }

func nullableTime(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	result := fromUnixNano(value.Int64)
	return &result
}

func translateNotFound(entity string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, entity)
	}
	return err
}

func requireChanged(entity string, result sql.Result) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read %s result: %w", entity, err)
	}
	if changed == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, entity)
	}
	return nil
}

func pageLimit(limit int) int {
	if limit <= 0 {
		return defaultPageSize
	}
	if limit > maxPageSize {
		return maxPageSize
	}
	return limit
}
