package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// A process-local optimization, never a persisted permission/checkpoint. See
// docs/audit-verification-cache.md for the trust boundary and invalidations.
const localAuditFullScanInterval = time.Minute

type auditCacheKey struct {
	connection any // modernc's *conn; retaining it also prevents address reuse
	version    int64
	recursive  int64
	domain     string
}

type localAuditCache struct {
	valid      bool
	key        auditCacheKey
	schema     int64
	head       model.AuditHead
	anchor     model.AuditAnchor
	fullScanAt time.Time
	fullScans  uint64
	suffixes   uint64
}

func (s *Store) configureAuditCache(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	digest, err := auditSchemaDigest(ctx, tx)
	if err != nil {
		return err
	}
	s.auditSchema = digest
	s.auditCacheSafe, err = auditAppendOnlySchema(ctx, tx)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.auditGate = make(chan struct{}, 1)
	return nil
}

// The on-disk startup schema is not itself a trust root. Only a real canonical
// audit table with the binary's unconditional UPDATE/DELETE guards can be
// cached. recursive_triggers on the writer also makes these guards apply to
// INSERT OR REPLACE, including replacements performed by another trigger.
func auditAppendOnlySchema(ctx context.Context, reader auditStateQuerier) (bool, error) {
	want := map[string]string{
		"audit_actions": strings.Replace(createAuditChainV2Table, "audit_actions_v2", "audit_actions", 1),
	}
	for _, op := range []string{"update", "delete"} {
		name := "audit_actions_no_" + op
		want[name] = fmt.Sprintf("CREATE TRIGGER %s BEFORE %s ON audit_actions BEGIN SELECT RAISE(ABORT, 'audit actions are append-only'); END", name, strings.ToUpper(op))
	}
	for name, expected := range want {
		var definition string
		if err := reader.QueryRowContext(ctx, `SELECT sql FROM sqlite_schema WHERE name=?`, name).Scan(&definition); errors.Is(err, sql.ErrNoRows) {
			return false, nil
		} else if err != nil {
			return false, err
		}
		normalized, err := normalizeSchemaSQL(definition)
		if err != nil {
			return false, err
		}
		canonical, err := normalizeSchemaSQL(expected)
		if err != nil {
			return false, err
		}
		if normalized != canonical {
			return false, nil
		}
	}
	return true, nil
}

func auditSchemaDigest(ctx context.Context, reader auditStateQuerier) ([sha256.Size]byte, error) {
	objects, err := readLegacySchema(ctx, reader)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	h := sha256.New()
	for _, object := range objects {
		writeAuditString(h, object.kind)
		writeAuditString(h, object.name)
		writeAuditString(h, object.table)
		writeAuditString(h, object.definition)
	}
	return [sha256.Size]byte(h.Sum(nil)), nil
}

// Only versions from the SAME physical connection are comparable. Store owns
// one writer connection: its own append-only application commits do not bump
// data_version, whereas every commit from another connection does. Replacing
// that connection invalidates the key even if SQLite reuses a counter value.
// No chain hashing or external work runs while this connection is held.
func (s *Store) auditCacheKey(ctx context.Context) (key auditCacheKey, resultErr error) {
	info, err := os.Stat(s.livePath)
	if err != nil {
		return key, fmt.Errorf("stat audit cache database: %w", err)
	}
	if !os.SameFile(s.liveFile, info) {
		return key, fmt.Errorf("%w: database file replaced; reopen Store before verification", ErrAuditStateChanged)
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return key, err
	}
	defer func() { resultErr = errors.Join(resultErr, conn.Close()) }()
	if err := conn.Raw(func(driverConn any) error {
		key.connection = driverConn
		return nil
	}); err != nil {
		return key, err
	}
	err = conn.QueryRowContext(ctx, `SELECT data_version, recursive_triggers FROM pragma_data_version, pragma_recursive_triggers`).Scan(&key.version, &key.recursive)
	key.domain = s.protocol.AuditDomain
	return key, err
}

// readLocalAuditChain runs within an already-pinned reader snapshot. The key
// sampled before opening that snapshot must equal both the cached key and a
// new writer-connection sample. This ordering prevents an external commit at
// snapshot creation from being mistaken for a verified prefix.
func (s *Store) readLocalAuditChain(ctx context.Context, tx *sql.Tx, before auditCacheKey, anchor model.AuditAnchor, schema int64, next *localAuditCache) (model.AuditHead, error) {
	current, err := s.auditCacheKey(ctx)
	if err != nil {
		return model.AuditHead{}, err
	}
	cache := s.localAudit
	now := time.Now() // monotonic elapsed time; independent of checkpoint/test clocks
	reuse := cache.valid && before == current && cache.key == before && cache.schema == schema &&
		sameVerifiedAuditAnchor(cache.anchor, anchor) && now.Sub(cache.fullScanAt) < localAuditFullScanInterval
	if reuse && verifyAuditHeadReference(ctx, tx, cache.head) != nil {
		reuse = false // regression, replacement or a missing boundary: scan from genesis
	}
	var head model.AuditHead
	if reuse {
		head, err = s.verifyAuditChainSince(ctx, tx, cache.head.ID, cache.head.SHA256)
		next.suffixes++
	} else {
		head, err = s.verifyAuditChain(ctx, tx)
		next.fullScans++
		next.fullScanAt = now
	}
	if err != nil {
		return model.AuditHead{}, err
	}
	// Invalidating a cache never discards its in-process rollback pin. A full
	// scan can prove a rewritten/shortened chain self-consistent, but cannot
	// authorize replacing a prefix already observed by this Store.
	if cache.head.ID > 0 {
		if err := verifyAuditHeadReference(ctx, tx, cache.head); err != nil {
			return model.AuditHead{}, fmt.Errorf("verified audit prefix: %w", err)
		}
	}
	// A schema change must not bless missing/altered append-only guards (or
	// new malicious triggers) as a trusted writer schema. On a mismatch the gate
	// still performs the original full verification, but cannot cache it.
	eligible := reuse
	if !reuse {
		digest, err := auditSchemaDigest(ctx, tx)
		if err != nil {
			return model.AuditHead{}, err
		}
		eligible = s.auditCacheSafe && before.recursive == 1 && bytes.Equal(digest[:], s.auditSchema[:])
	}
	next.valid, next.key, next.schema = eligible, before, schema
	next.head = model.AuditHead{ID: head.ID, SHA256: bytes.Clone(head.SHA256)}
	next.anchor = anchor
	next.anchor.AuditSHA256 = bytes.Clone(anchor.AuditSHA256)
	next.anchor.Receipt = bytes.Clone(anchor.Receipt)
	return head, nil
}
