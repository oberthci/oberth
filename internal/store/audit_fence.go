package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

// ErrAuditStateChanged refuses a stale verification, never granting a gate.
// Only ErrAuditVerificationBusy permits an automatic fresh attempt; changed
// proofs, replaced files and regressed heads must remain fail-closed.
var ErrAuditStateChanged = errors.New("store: audit state changed during verification; retry")

// ErrAuditVerificationBusy classifies concurrent commits as transient. Callers
// must repeat ALL verification, including external proof checks, before allowing
// a mutation. It also matches ErrAuditStateChanged for existing callers.
var ErrAuditVerificationBusy = fmt.Errorf("%w: transient verification contention", ErrAuditStateChanged)

// These are cheap local retries within one external proof. The Manager waits
// and retries fresh proofs under its overall gate deadline when they run out.
const auditFenceExtraRounds = 2

// Keep this exact reader connection across every snapshot and fence: only its
// own data_version values are comparable. It never writes to the live WAL.
func (s *Store) openAuditReader(ctx context.Context) (*sql.DB, *sql.Conn, error) {
	db, err := s.openLiveReaders(ctx, 1)
	if err != nil {
		return nil, nil, err
	}
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
	version, head, anchor, err := readAuditVerification(ctx, conn, verify)
	if err != nil {
		return model.AuditHead{}, model.AuditAnchor{}, err
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
	for round := 0; ; round++ {
		changed, err := s.auditVerificationFence(ctx, conn, head, version)
		if err != nil {
			return model.AuditHead{}, model.AuditAnchor{}, err
		}
		if !changed {
			return head, anchor, nil
		}
		if round == auditFenceExtraRounds {
			return model.AuditHead{}, model.AuditAnchor{}, fmt.Errorf("%w: local re-verification rounds exhausted", ErrAuditVerificationBusy)
		}
		// The writer fence has ended. Re-verify protected state on a new read
		// snapshot, using the same reader and a fresh counter. The local gate
		// independently checks whether its prefix cache is still trusted.
		// Unrelated writes during external I/O need not invalidate an unchanged
		// verified result.
		nextVersion, nextHead, nextAnchor, err := readAuditVerification(ctx, conn, verify)
		if err != nil {
			return model.AuditHead{}, model.AuditAnchor{}, err
		}
		if !sameVerifiedAuditAnchor(nextAnchor, anchor) {
			return model.AuditHead{}, model.AuditAnchor{}, fmt.Errorf("%w: re-verified head or checkpoint differs from external proof", ErrAuditStateChanged)
		}
		if nextHead.ID != head.ID || !bytes.Equal(nextHead.SHA256, head.SHA256) {
			return model.AuditHead{}, model.AuditAnchor{}, auditFenceHeadChanged(head, nextHead)
		}
		version = nextVersion
	}
}

func readAuditVerification(ctx context.Context, conn *sql.Conn, verify func(*sql.Tx) (model.AuditHead, model.AuditAnchor, error)) (int64, model.AuditHead, model.AuditAnchor, error) {
	version, err := auditDataVersion(ctx, conn)
	if err != nil {
		return 0, model.AuditHead{}, model.AuditAnchor{}, err
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, model.AuditHead{}, model.AuditAnchor{}, fmt.Errorf("begin audit verification snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	head, anchor, err := verify(tx)
	if err != nil {
		return 0, model.AuditHead{}, model.AuditAnchor{}, err
	}
	if err := tx.Commit(); err != nil {
		return 0, model.AuditHead{}, model.AuditAnchor{}, fmt.Errorf("finish audit verification snapshot: %w", err)
	}
	return version, head, anchor, nil
}

func sameVerifiedAuditAnchor(a, b model.AuditAnchor) bool {
	return a.ID == b.ID && a.AuditID == b.AuditID && bytes.Equal(a.AuditSHA256, b.AuditSHA256) &&
		a.TSAURL == b.TSAURL && bytes.Equal(a.Receipt, b.Receipt) && a.AnchoredAt.Equal(b.AnchoredAt) && a.CreatedAt.Equal(b.CreatedAt)
}

// This reservation contains only bounded local comparisons; no callback,
// hashing or network I/O. It ends before any retry begins.
func (s *Store) auditVerificationFence(ctx context.Context, conn *sql.Conn, head model.AuditHead, version int64) (bool, error) {
	fence, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin audit verification fence: %w", err)
	}
	defer func() { _ = fence.Rollback() }()
	current, err := readAuditHead(ctx, fence)
	if err != nil {
		return false, err
	}
	if current.ID != head.ID || !bytes.Equal(current.SHA256, head.SHA256) {
		return false, auditFenceHeadChanged(head, current)
	}
	currentVersion, err := auditDataVersion(ctx, conn)
	if err != nil {
		return false, err
	}
	if currentVersion != version {
		return true, nil
	}
	if err := fence.Commit(); err != nil {
		return false, fmt.Errorf("commit audit verification fence: %w", err)
	}
	return false, nil
}

func auditFenceHeadChanged(verified, current model.AuditHead) error {
	kind := ErrAuditStateChanged
	if current.ID > verified.ID {
		kind = ErrAuditVerificationBusy
	}
	return fmt.Errorf("%w: verified head %d differs from current head %d", kind, verified.ID, current.ID)
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
