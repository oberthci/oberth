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

// ErrAuditVerificationBusy classifies advancing audit heads as transient. Callers
// must repeat ALL verification, including external proof checks, before allowing
// a mutation. It also matches ErrAuditStateChanged for existing callers.
var ErrAuditVerificationBusy = fmt.Errorf("%w: transient verification contention", ErrAuditStateChanged)

// Bound re-verification for untrusted outside commits or altered schemas.
// Exhaustion fails closed; only an advancing head permits a Manager retry.
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
	proof, head, anchor, err := s.readAuditVerification(ctx, conn, verify)
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
		changed, err := s.auditVerificationFence(ctx, conn, head, anchor, proof)
		if err != nil {
			return model.AuditHead{}, model.AuditAnchor{}, err
		}
		if !changed {
			return head, anchor, nil
		}
		if round == auditFenceExtraRounds {
			return model.AuditHead{}, model.AuditAnchor{}, fmt.Errorf("%w: local re-verification rounds exhausted", ErrAuditStateChanged)
		}
		// The writer fence has ended. Re-verify protected state on a new read
		// snapshot, using the same reader and a fresh counter. The local gate
		// independently checks whether its prefix cache is still trusted.
		// Unrelated writes during external I/O need not invalidate an unchanged
		// verified result.
		nextProof, nextHead, nextAnchor, err := s.readAuditVerification(ctx, conn, verify)
		if err != nil {
			return model.AuditHead{}, model.AuditAnchor{}, err
		}
		if !sameVerifiedAuditAnchor(nextAnchor, anchor) {
			return model.AuditHead{}, model.AuditAnchor{}, fmt.Errorf("%w: re-verified head or checkpoint differs from external proof", ErrAuditStateChanged)
		}
		if nextHead.ID != head.ID || !bytes.Equal(nextHead.SHA256, head.SHA256) {
			return model.AuditHead{}, model.AuditAnchor{}, auditFenceHeadChanged(head, nextHead)
		}
		proof = nextProof
	}
}

// The primary counter distinguishes trusted Store commits from arbitrary outside
// edits. The reader counter alone cannot make that distinction. Both samples
// precede the snapshot; the fence compares them on their original connections.
type auditFenceProof struct {
	readerVersion int64
	writer        auditCacheKey
	schema        int64
	trustedSchema bool
}

func (s *Store) readAuditVerification(ctx context.Context, conn *sql.Conn, verify func(*sql.Tx) (model.AuditHead, model.AuditAnchor, error)) (proof auditFenceProof, head model.AuditHead, anchor model.AuditAnchor, err error) {
	proof.writer, err = s.auditCacheKey(ctx)
	if err != nil {
		return
	}
	proof.readerVersion, err = auditDataVersion(ctx, conn)
	if err != nil {
		return
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return proof, head, anchor, fmt.Errorf("begin audit verification snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err = tx.QueryRowContext(ctx, `PRAGMA schema_version`).Scan(&proof.schema); err != nil {
		return
	}
	digest, err := auditSchemaDigest(ctx, tx)
	if err != nil {
		return proof, head, anchor, err
	}
	proof.trustedSchema = s.auditCacheSafe && digest == s.auditSchema && proof.writer.recursive == 1
	head, anchor, err = verify(tx)
	if err != nil {
		return
	}
	if err = tx.Commit(); err != nil {
		err = fmt.Errorf("finish audit verification snapshot: %w", err)
	}
	return
}

func sameVerifiedAuditAnchor(a, b model.AuditAnchor) bool {
	return a.ID == b.ID && a.AuditID == b.AuditID && bytes.Equal(a.AuditSHA256, b.AuditSHA256) &&
		a.TSAURL == b.TSAURL && bytes.Equal(a.Receipt, b.Receipt) && a.AnchoredAt.Equal(b.AnchoredAt) && a.CreatedAt.Equal(b.CreatedAt)
}

// This reservation contains only bounded local comparisons; no callback,
// hashing or network I/O. It ends before any retry begins.
func (s *Store) auditVerificationFence(ctx context.Context, conn *sql.Conn, head model.AuditHead, anchor model.AuditAnchor, proof auditFenceProof) (changed bool, resultErr error) {
	writer, err := s.db.Conn(ctx)
	if err != nil {
		return false, fmt.Errorf("connect audit verification fence: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, writer.Close()) }()
	fence, err := writer.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin audit verification fence: %w", err)
	}
	defer func() { _ = fence.Rollback() }()
	key, err := s.auditCacheKeyOnConnection(ctx, writer, fence)
	if err != nil {
		return false, err
	}
	current, err := readAuditHead(ctx, fence)
	if err != nil {
		return false, err
	}
	// Check the old boundary even on an advance: replacing the verified prefix
	// must never be classified as an ordinary append race.
	if current.ID >= head.ID {
		if err := verifyAuditHeadReference(ctx, fence, head); err != nil {
			return false, fmt.Errorf("%w: %w", ErrAuditStateChanged, err)
		}
	}
	if current.ID != head.ID || !bytes.Equal(current.SHA256, head.SHA256) {
		return false, auditFenceHeadChanged(head, current)
	}
	currentVersion, err := auditDataVersion(ctx, conn)
	if err != nil {
		return false, err
	}
	if currentVersion != proof.readerVersion || key != proof.writer {
		currentAnchor, err := scanAuditAnchor(fence.QueryRowContext(ctx, `SELECT id, audit_id, audit_sha256, tsa_url, receipt, anchored_at, created_at FROM audit_anchors ORDER BY id DESC LIMIT 1`))
		if errors.Is(err, ErrNotFound) {
			err = nil
		}
		if err != nil {
			return false, err
		}
		if !sameVerifiedAuditAnchor(currentAnchor, anchor) {
			return false, fmt.Errorf("%w: checkpoint differs from external proof", ErrAuditStateChanged)
		}
		var schema int64
		if err := fence.QueryRowContext(ctx, `PRAGMA schema_version`).Scan(&schema); err != nil {
			return false, err
		}
		// Only canonical-schema commits on our own physical writer are trusted.
		// Foreign edits or altered guards need fresh full verification, even when
		// head/digest/receipt are unchanged (prefix and pending-row tampering).
		if key != proof.writer || !proof.trustedSchema || schema != proof.schema {
			return true, nil
		}
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
