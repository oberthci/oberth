package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestConfiguredProtocolPreservesHistoricalState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "instance.sqlite")
	legacy := ProtocolConfig{SchemaIdentity: "sample-oberth-schema-v1", AuditDomain: "sample-oberth-audit-v1"}
	opts := Options{Protocol: legacy, Now: func() time.Time { return time.Unix(100, 0) }}
	s, err := Open(ctx, path, opts)
	if err != nil {
		t.Fatal(err)
	}
	spec := model.AuditActionSpec{Actor: "operator", Action: "fixture", ResourceType: "instance", ResourceID: "original", Details: "{}"}
	action, err := s.AppendAuditAction(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	// Independent protocol encoder pins the historical prefix including NUL,
	// field lengths and integer byte order; configurable code must match it.
	hash := sha256.New()
	hash.Write([]byte("sample-oberth-audit-v1\x00"))
	integer := func(v int64) { var b [8]byte; binary.BigEndian.PutUint64(b[:], uint64(v)); hash.Write(b[:]) }
	field := func(v []byte) { integer(int64(len(v))); hash.Write(v) }
	integer(action.ID)
	field(make([]byte, 32))
	for _, v := range []string{spec.Actor, spec.Action, spec.ResourceType, spec.ResourceID, spec.Details} {
		field([]byte(v))
	}
	integer(action.CreatedAt.UnixNano())
	if !bytes.Equal(action.SHA256, hash.Sum(nil)) {
		t.Fatal("historical hash protocol changed")
	}
	var identity, ddl string
	if err := s.db.QueryRow(`SELECT identity FROM schema_identity`).Scan(&identity); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_schema WHERE name='schema_identity'`).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	if identity != legacy.SchemaIdentity || !strings.Contains(ddl, "identity = '"+legacy.SchemaIdentity+"'") {
		t.Fatal("configured historical schema identity was not preserved")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if db, err := InspectCurrent(ctx, path, Options{}); err == nil {
		db.Close()
		t.Fatal("neutral defaults adopted a historical identity")
	}
	wrong := opts
	wrong.Protocol.AuditDomain = "another-audit-v1"
	if db, err := InspectCurrent(ctx, path, wrong); err == nil {
		db.Close()
		t.Fatal("incorrect audit domain was accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed read-only inspection modified the database")
	}
	s, err = OpenCurrent(ctx, path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	head, err := s.VerifyAuditState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if head.ID != action.ID || !bytes.Equal(head.SHA256, action.SHA256) {
		t.Fatal("historical head changed on reopen")
	}
	next, err := s.AppendAuditAction(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(next.PreviousSHA256, action.SHA256) {
		t.Fatal("append did not continue original chain")
	}
	if _, err := s.VerifyAuditStateSince(ctx, action.ID, action.SHA256); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.VerifyAuditMutationStateUnanchored(ctx, nil, nil, func(model.AuditHead, model.AuditAnchor) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestConfiguredProtocolRejectsMismatchedConcurrentWriter(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "instance.sqlite")
	first, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	alternate := DefaultProtocolConfig()
	alternate.AuditDomain = "different-audit-v1"
	second, err := OpenAdminClient(ctx, path, Options{Protocol: alternate})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	spec := model.AuditActionSpec{Actor: "operator", Action: "fixture", ResourceType: "instance", Details: "{}"}
	if _, err := first.AppendAuditAction(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := second.AppendAuditAction(ctx, spec); err == nil {
		t.Fatal("mismatched writer appended to existing chain")
	}
	if head, err := first.VerifyAuditChain(ctx); err != nil || head.ID != 1 {
		t.Fatalf("original chain changed: %+v %v", head, err)
	}
}

func TestConfiguredProtocolValidationAndBootstrapBytes(t *testing.T) {
	for _, p := range []ProtocolConfig{{SchemaIdentity: "only-identity"}, {AuditDomain: "only-domain"}, {SchemaIdentity: "bad'identity", AuditDomain: "audit"}, {SchemaIdentity: "schema", AuditDomain: "bad\x00domain"}} {
		if err := p.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid protocol accepted: %+v: %v", p, err)
		}
	}
	p := ProtocolConfig{SchemaIdentity: "sample-oberth-schema-v1", AuditDomain: "sample-oberth-audit-v1"}
	s := &Store{protocol: p}
	expected := strings.ReplaceAll(migrations[0].sql, "'oberth-schema-v1'", "'sample-oberth-schema-v1'")
	if s.migrationSQL(migrations[0]) != expected {
		t.Fatal("bootstrap SQL changed beyond exact identity operands")
	}
	if got := strings.Count(s.migrationSQL(migrations[0]), "sample-oberth-schema-v1"); got != 2 {
		t.Fatalf("identity operands=%d", got)
	}
	for _, m := range migrations[1:] {
		if s.migrationSQL(m) != m.sql {
			t.Fatalf("shipped migration %d changed", m.version)
		}
	}
}

func TestConfiguredProtocolPreservesExactPredecessorMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "predecessor.sqlite")
	protocol := ProtocolConfig{SchemaIdentity: "sample-oberth-schema-v1", AuditDomain: "sample-oberth-audit-v1"}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{`PRAGMA journal_mode=WAL`, createMigrationLedger, (&Store{protocol: protocol}).migrationSQL(migrations[0]), `INSERT INTO schema_migrations(version,applied_at) VALUES(1,1)`, `INSERT INTO audit_actions(id,actor,action,resource_type,resource_id,details,created_at) VALUES(1,'operator','fixture','instance','original','{}',1)`} {
		if _, err := raw.Exec(statement); err != nil {
			raw.Close()
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	opts := Options{Protocol: protocol}
	head, err := InspectLegacyV1(ctx, path, opts)
	if err != nil {
		t.Fatal(err)
	}
	migrated, err := MigrateLegacyV1(ctx, path, head, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	actual, err := migrated.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if actual.ID != head.ID || !bytes.Equal(actual.SHA256, head.SHA256) {
		t.Fatal("authorized predecessor head changed")
	}
	var identity string
	if err := migrated.db.QueryRow(`SELECT identity FROM schema_identity`).Scan(&identity); err != nil {
		t.Fatal(err)
	}
	if identity != protocol.SchemaIdentity {
		t.Fatal("predecessor migration rewrote deployment identity")
	}
	var version int
	if err := migrated.db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("authorized migration advanced beyond exact ratified version: %d", version)
	}
}
