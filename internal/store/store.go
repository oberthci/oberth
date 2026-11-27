package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/stateport"
	_ "modernc.org/sqlite" // Registers the embedded database driver.
)

// Options is copied at construction; Clock is required for durable timestamps.
type Options struct {
	Protocol    stateport.Protocol
	BusyTimeout time.Duration
	Clock       func() time.Time
}

// Store owns independent writer, observation and reserved live reader pools.
type Store struct {
	refusals                                                    receiveRefusals
	metrics                                                     storeMetrics
	notifications                                               stateport.ChangeHub
	pendingChanges                                              map[stateport.Change]struct{}
	backupMu                                                    sync.RWMutex
	lastBackup                                                  time.Time
	writes                                                      chan struct{}
	path                                                        string
	options                                                     Options
	writer, readers, auditReaders, authReaders, readiness, idle *sql.DB
	protocols                                                   [2]chan struct{}
	uses                                                        chan string
	usesDone                                                    chan struct{}
	stopUses                                                    context.CancelFunc
	issueFacts                                                  issueFacts
	issueWrites                                                 []int64  // held by the write slot
	auditSpan                                                   [2]int64 // held by the write slot
}

// Create exclusively creates the current baseline. It never upgrades a database.
func Create(ctx context.Context, path string, options Options) (_ *Store, resultErr error) {
	path, options, err := configuration(path, options)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if errors.Is(err, os.ErrExist) {
		return nil, refuse(exists, err)
	}
	if err != nil {
		return nil, databaseError(err)
	}
	if err := file.Close(); err != nil {
		return nil, databaseError(err)
	}
	complete := false
	defer func() {
		if !complete {
			resultErr = errors.Join(resultErr, os.Remove(path))
		}
	}()
	db := pool(path, "rw", options, 1)
	defer func() { resultErr = errors.Join(resultErr, db.Close()) }()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, databaseError(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, baseline); err != nil {
		return nil, databaseError(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO protocol VALUES(1,?,?,?)`, Schema, options.Protocol.SchemaIdentity,
		options.Protocol.AuditDomain); err != nil {
		return nil, databaseError(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_head VALUES(1,0,?)`, genesis()); err != nil {
		return nil, databaseError(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, databaseError(err)
	}
	complete = true
	return Open(ctx, path, options)
}

// Open requires an existing database and applies supported forward migrations.
func Open(ctx context.Context, path string, options Options) (*Store, error) {
	return open(ctx, path, options, currentSchema())
}

func open(ctx context.Context, path string, options Options, definition schemaDefinition) (*Store, error) {
	path, options, err := configuration(path, options)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, databaseError(err)
	}
	if !info.Mode().IsRegular() {
		return nil, refuse(invalid, nil)
	}
	if err := sweepStaging(filepath.Dir(path)); err != nil {
		return nil, err
	}
	s := &Store{path: path, options: options, writes: make(chan struct{}, 1)}
	s.writer = pool(path, "rw", options, 1)
	s.readers = pool(path, "ro", options, 8)
	s.auditReaders = pool(path, "ro", options, 2)
	s.authReaders = pool(path, "ro", options, 4)
	s.readiness = pool(path, "ro", options, 1)
	s.idle = pool(path, "ro", options, 1)
	s.protocols = [2]chan struct{}{make(chan struct{}, 3), make(chan struct{}, 3)}
	if err := s.prepareSchema(ctx, definition); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	if err := s.loadLastBackup(ctx); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	worker, stop := context.WithCancel(context.WithoutCancel(ctx))
	s.uses = make(chan string, 64)
	s.usesDone = make(chan struct{})
	s.stopUses = stop
	go s.recordUses(worker)
	return s, nil
}

func configuration(path string, o Options) (string, Options, error) {
	if path == "" || strings.ContainsAny(path, "?\x00#") || strings.HasPrefix(path, "file:") || o.Clock == nil {
		return "", o, refuse(invalid, nil)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", o, refuse(invalid, err)
	}
	if err := o.Protocol.Validate(); err != nil {
		return "", o, err
	}
	if o.BusyTimeout == 0 {
		o.BusyTimeout = 5 * time.Second
	}
	if o.BusyTimeout < 0 {
		return "", o, refuse(invalid, nil)
	}
	return path, o, nil
}

func pool(path, mode string, o Options, size int) *sql.DB {
	query := url.Values{
		"mode":    {mode},
		"_pragma": {"foreign_keys(1)", "busy_timeout(" + strconv.FormatInt(o.BusyTimeout.Milliseconds(), 10) + ")"},
	}
	if mode == "rw" {
		query["_pragma"][1] = "busy_timeout(0)"
		query.Add("_pragma", "journal_mode(WAL)")
		query.Add("_pragma", "synchronous(FULL)")
		query.Set("_txlock", "immediate")
	} else {
		query.Add("_pragma", "query_only(1)")
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String()
	db, _ := sql.Open("sqlite", dsn)
	db.SetMaxOpenConns(size)
	return db
}

// Close drains connections without holding a mutex needed by callers.
func (s *Store) Close() error {
	if s.stopUses != nil {
		s.stopUses()
		<-s.usesDone
	}
	var errs []error
	for _, db := range []*sql.DB{s.readers, s.auditReaders, s.authReaders, s.readiness, s.idle} {
		if db != nil {
			errs = append(errs, db.Close())
		}
	}
	if s.writer != nil {
		errs = append(errs, s.writer.Close())
	}
	return errors.Join(errs...)
}

func checkProtocol(ctx context.Context, db protocolReader, o stateport.Options) error {
	number, err := readBoundProtocol(ctx, db, o.Protocol)
	if err != nil {
		return err
	}
	if number > o.Schema {
		return refuse(newerSchema, nil)
	}
	if number < o.Schema {
		return schemaVersionRefusal(olderSchema, number)
	}
	return nil
}

type protocolReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readBoundProtocol(ctx context.Context, db protocolReader, protocol stateport.Protocol) (int, error) {
	var number int
	var identity, domain string
	err := db.QueryRowContext(ctx, `SELECT schema_number,identity,domain FROM protocol WHERE singleton=1`).Scan(&number, &identity, &domain)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, refuse(schema, err)
	}
	if err != nil {
		return 0, databaseError(err)
	}
	if identity != protocol.SchemaIdentity || domain != protocol.AuditDomain {
		return 0, refuse(foreignProtocol, &ProtocolMismatchError{})
	}
	return number, nil
}

// DefaultOptions selects the explicit protocol for a fresh installation.
func DefaultOptions(clock func() time.Time) Options {
	return Options{Protocol: stateport.DefaultProtocol(), Clock: clock}
}

// ProtocolMismatchError distinguishes installation binding from schema version.
// The offline recovery helper uses it to explain a foreign installation backup.
type ProtocolMismatchError struct{}

func (*ProtocolMismatchError) Error() string { return "database protocol binding differs" }
