package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

// ErrAuditStateChanged is a retryable refusal, never a successful gate. One
// attempt is made per call so a busy writer cannot cause unbounded retries or
// repeated external side effects inside a request.
var ErrAuditStateChanged = errors.New("store: audit state changed during verification; retry")

// openAuditReader creates a private, deferred, read-only connection to the live
// WAL. Its data_version must be compared only on this exact connection. Since
// it never writes, committed database changes (including this Store's writes)
// change its observed version. Never inherit _txlock=immediate or immutable=1.
func (s *Store) openAuditReader(ctx context.Context) (*sql.DB, *sql.Conn, error) {
	var path string
	if err := s.db.QueryRowContext(ctx, `SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		return nil, nil, fmt.Errorf("locate audit verification database: %w", err)
	}
	if path == "" {
		return nil, nil, fmt.Errorf("%w: audit verification requires a file-backed WAL database", ErrInvalid)
	}
	db, err := sql.Open("sqlite", currentDatabaseDSN(path, "ro", false))
	if err != nil {
		return nil, nil, fmt.Errorf("open audit verification reader: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("connect audit verification reader: %w", err)
	}
	return db, conn, nil
}

func auditDataVersion(ctx context.Context, conn *sql.Conn) (int64, error) {
	var version int64
	if err := conn.QueryRowContext(ctx, `PRAGMA data_version`).Scan(&version); err != nil {
		return 0, fmt.Errorf("read audit verification data version: %w", err)
	}
	return version, nil
}

func (s *Store) verifyAuditWithFence(
	ctx context.Context,
	verify func(*sql.Tx) (model.AuditHead, model.AuditAnchor, error),
	external func(model.AuditHead, model.AuditAnchor) error,
) (head model.AuditHead, anchor model.AuditAnchor, resultErr error) {
	if s.readOnly {
		return head, anchor, fmt.Errorf("%w: mutation gate requires a writable Store", ErrInvalidState)
	}
	db, conn, err := s.openAuditReader(ctx)
	if err != nil {
		return head, anchor, err
	}
	defer func() { resultErr = errors.Join(resultErr, conn.Close(), db.Close()) }()
	// Read the counter before the first snapshot read. A commit in between can
	// only cause a conservative refusal, never acceptance of a stale snapshot.
	version, err := auditDataVersion(ctx, conn)
	if err != nil {
		return head, anchor, err
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return head, anchor, fmt.Errorf("begin audit verification snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	head, anchor, err = verify(tx)
	if err != nil {
		return model.AuditHead{}, model.AuditAnchor{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.AuditHead{}, model.AuditAnchor{}, fmt.Errorf("finish audit verification snapshot: %w", err)
	}
	// The result is immutable to the callback: it gets independent slices. The
	// read transaction is already over, so slow I/O pins neither a writer nor WAL
	// snapshot pages. Failure remains fail-closed even if external writes occur.
	callbackHead, callbackAnchor := head, anchor
	callbackHead.SHA256 = bytes.Clone(head.SHA256)
	callbackAnchor.AuditSHA256 = bytes.Clone(anchor.AuditSHA256)
	callbackAnchor.Receipt = bytes.Clone(anchor.Receipt)
	if err := external(callbackHead, callbackAnchor); err != nil {
		return model.AuditHead{}, model.AuditAnchor{}, err
	}
	// Store writer DSNs always use BEGIN IMMEDIATE. No callback, hashing or
	// network I/O occurs under this reservation. Other writers remain excluded
	// from these comparisons through commit.
	fence, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.AuditHead{}, model.AuditAnchor{}, fmt.Errorf("begin audit verification fence: %w", err)
	}
	defer func() { _ = fence.Rollback() }()
	current, err := readAuditHead(ctx, fence)
	if err != nil {
		return model.AuditHead{}, model.AuditAnchor{}, err
	}
	if current.ID != head.ID || !bytes.Equal(current.SHA256, head.SHA256) {
		return model.AuditHead{}, model.AuditAnchor{}, fmt.Errorf("%w: verified head %d differs from current head %d", ErrAuditStateChanged, head.ID, current.ID)
	}
	currentVersion, err := auditDataVersion(ctx, conn)
	if err != nil {
		return model.AuditHead{}, model.AuditAnchor{}, err
	}
	if currentVersion != version {
		// Head equality alone misses prefix, pending-publication, receipt, anchor,
		// schema, and change-then-restore commits. Reject all such changes too.
		return model.AuditHead{}, model.AuditAnchor{}, fmt.Errorf("%w: database changed with an unchanged head", ErrAuditStateChanged)
	}
	if err := fence.Commit(); err != nil {
		return model.AuditHead{}, model.AuditAnchor{}, fmt.Errorf("commit audit verification fence: %w", err)
	}
	return head, anchor, nil
}

// withAuditReadSnapshot keeps full-chain CPU work off the primary connection
// during periodic verification too. Inspection handles already own a private,
// read-only snapshot and retain their original immutable connection settings.
func (s *Store) withAuditReadSnapshot(ctx context.Context, verify func(*sql.Tx) (model.AuditHead, error)) (head model.AuditHead, resultErr error) {
	var db *sql.DB
	var conn *sql.Conn
	var err error
	if s.readOnly {
		conn, err = s.db.Conn(ctx)
	} else {
		db, conn, err = s.openAuditReader(ctx)
	}
	if err != nil {
		return head, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, conn.Close())
		if db != nil {
			resultErr = errors.Join(resultErr, db.Close())
		}
	}()
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return head, fmt.Errorf("begin audit read snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	head, err = verify(tx)
	if err != nil {
		return model.AuditHead{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.AuditHead{}, fmt.Errorf("finish audit read snapshot: %w", err)
	}
	return head, nil
}
