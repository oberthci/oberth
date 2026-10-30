package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite"

	"github.com/oberthci/oberth/internal/model"
)

func localGate(t testing.TB, s *Store) model.AuditHead {
	t.Helper()
	head, _, err := s.VerifyLocalAuditMutationState(t.Context(), model.AuditHead{}, acceptAuditMutationState)
	if err != nil {
		t.Fatal(err)
	}
	return head
}

func appendCacheAction(t testing.TB, s *Store) model.AuditAction {
	t.Helper()
	action, err := s.AppendAuditAction(t.Context(), model.AuditActionSpec{Actor: "test", Action: "cache.append", ResourceType: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return action
}

func TestLocalAuditCacheVerifiesOnlyAppendedSuffix(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	seedAuditMetricRows(t, s, 128, now)
	localGate(t, s)
	for range 8 {
		action := appendCacheAction(t, s)
		head := localGate(t, s)
		if head.ID != action.ID || !bytes.Equal(head.SHA256, action.SHA256) {
			t.Fatalf("verified head = %+v, want %+v", head, action)
		}
	}
	if !s.localAudit.valid || s.localAudit.fullScans != 1 || s.localAudit.suffixes != 8 {
		t.Fatalf("unexpected cache: %+v", s.localAudit)
	}
}

// Perform a realistic outside edit, restoring both the schema definition and
// its cookie. The process-owned data_version must still force a full scan.
func outsideAuditEdit(t *testing.T, s *Store, statement string) {
	t.Helper()
	db, err := sql.Open("sqlite", s.livePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var schema int64
	if err := tx.QueryRowContext(t.Context(), `PRAGMA schema_version`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	var triggers []string
	for _, name := range []string{"audit_actions_no_update", "audit_actions_no_delete"} {
		var definition string
		if err := tx.QueryRowContext(t.Context(), `SELECT sql FROM sqlite_schema WHERE name=?`, name).Scan(&definition); err != nil {
			t.Fatal(err)
		}
		triggers = append(triggers, definition)
		if _, err := tx.ExecContext(t.Context(), `DROP TRIGGER `+name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(t.Context(), statement); err != nil {
		t.Fatal(err)
	}
	for _, definition := range triggers {
		if _, err := tx.ExecContext(t.Context(), definition); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(t.Context(), fmt.Sprintf(`PRAGMA schema_version=%d`, schema)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestLocalAuditCacheRejectsTamperingAndRollback(t *testing.T) {
	for _, tc := range []struct{ name, statement, want string }{
		{"verified-prefix", `UPDATE audit_actions SET details='{"forged":true}' WHERE id=1`, "audit action 1 hash is invalid"},
		{"missing-middle", `DELETE FROM audit_actions WHERE id=2`, "sequence is broken"},
		{"valid-shortened-restore", `DELETE FROM audit_actions WHERE id=3`, "verified audit prefix"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			s := testStore(t, &now)
			seedAuditMetricRows(t, s, 3, now)
			localGate(t, s)
			localGate(t, s) // prove the cache was used before the edit
			outsideAuditEdit(t, s, tc.statement)
			called := false
			_, _, err := s.VerifyLocalAuditMutationState(t.Context(), model.AuditHead{}, func(model.AuditHead, model.AuditAnchor) error { called = true; return nil })
			if err == nil || !strings.Contains(err.Error(), tc.want) || called || s.localAudit.valid {
				t.Fatalf("tamper gate: callback=%v cache-valid=%v err=%v", called, s.localAudit.valid, err)
			}
			// Failure cannot erase the old pin and bless a valid shorter chain.
			if tc.name == "valid-shortened-restore" {
				if _, _, err := s.VerifyLocalAuditMutationState(t.Context(), model.AuditHead{}, acceptAuditMutationState); err == nil {
					t.Fatal("retry forgot the prefix pin")
				}
			}
		})
	}
}

func TestLocalAuditCacheRejectsForgedNewSuffix(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	head := seedAuditMetricRows(t, s, 3, now)
	localGate(t, s)
	if _, err := s.db.ExecContext(t.Context(), `INSERT INTO audit_actions(id, actor, action, resource_type, resource_id, details, previous_sha256, sha256, created_at) VALUES(4,'test','forged','test','', '{}', ?, zeroblob(32), ?)`, head.SHA256, now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.VerifyLocalAuditMutationState(t.Context(), model.AuditHead{}, acceptAuditMutationState)
	if err == nil || !strings.Contains(err.Error(), "audit action 4 hash is invalid") || s.localAudit.valid {
		t.Fatalf("forged suffix accepted: %v", err)
	}
}

func TestLocalAuditCacheInvalidations(t *testing.T) {
	for _, change := range []string{"expiry", "invalid", "key", "connection", "external-append", "schema", "anchor"} {
		t.Run(change, func(t *testing.T) {
			now := time.Now()
			s := testStore(t, &now)
			seedAuditMetricRows(t, s, 3, now)
			head := localGate(t, s)
			switch change {
			case "expiry":
				s.localAudit.fullScanAt = time.Now().Add(-localAuditFullScanInterval)
			case "invalid":
				s.localAudit.valid = false
			case "key":
				s.localAudit.key.domain = "other-domain"
			case "connection":
				s.db.SetMaxIdleConns(0)
				s.db.SetMaxIdleConns(1)
			case "external-append":
				other, err := OpenAdminClient(t.Context(), s.livePath, Options{})
				if err != nil {
					t.Fatal(err)
				}
				appendCacheAction(t, other)
				if err := other.Close(); err != nil {
					t.Fatal(err)
				}
			case "schema":
				if _, err := s.db.ExecContext(t.Context(), `CREATE TABLE cache_schema_change(id INTEGER)`); err != nil {
					t.Fatal(err)
				}
			case "anchor":
				if _, err := s.RecordAuditAnchor(t.Context(), model.AuditAnchorSpec{AuditID: head.ID, AuditSHA256: head.SHA256, TSAURL: "test", Receipt: []byte("receipt"), AnchoredAt: now}); err != nil {
					t.Fatal(err)
				}
			}
			localGate(t, s)
			if s.localAudit.fullScans != 2 || s.localAudit.suffixes != 0 {
				t.Fatalf("%s failed to force a full scan: %+v", change, s.localAudit)
			}
			if change == "schema" {
				localGate(t, s)
				if s.localAudit.valid || s.localAudit.fullScans != 3 {
					t.Fatal("changed schema was trusted as a new baseline")
				}
			}
		})
	}
}

func TestLocalAuditCacheDoesNotPublishFailedFence(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	seedAuditMetricRows(t, s, 3, now)
	localGate(t, s)
	old := s.localAudit.head
	appended := make(chan error, 1)
	start := make(chan struct{})
	go func() {
		<-start
		_, err := s.AppendAuditAction(t.Context(), model.AuditActionSpec{Actor: "concurrent", Action: "append", ResourceType: "test"})
		appended <- err
	}()
	_, _, err := s.VerifyLocalAuditMutationState(t.Context(), model.AuditHead{}, func(model.AuditHead, model.AuditAnchor) error {
		close(start)
		return <-appended
	})
	if !errors.Is(err, ErrAuditStateChanged) || s.localAudit.valid || s.localAudit.head.ID != old.ID {
		t.Fatalf("moved-head gate cached permission: %+v, %v", s.localAudit, err)
	}
	if got := localGate(t, s); got.ID != old.ID+1 || s.localAudit.fullScans != 2 {
		t.Fatalf("fresh retry did not fully verify append: %+v", s.localAudit)
	}
}

func TestLocalAuditCacheRechecksOutsidePrefixEditDuringCallback(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	seedAuditMetricRows(t, s, 3, now)
	localGate(t, s)
	calls := 0
	_, _, err := s.VerifyLocalAuditMutationState(t.Context(), model.AuditHead{}, func(model.AuditHead, model.AuditAnchor) error {
		calls++
		outsideAuditEdit(t, s, `UPDATE audit_actions SET actor='forged' WHERE id=1`)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "audit action 1 hash is invalid") || calls != 1 {
		t.Fatalf("prefix changed during external work: calls=%d err=%v", calls, err)
	}
}

func TestLocalAuditCacheReusesPrefixAfterOwnUnrelatedWrite(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	seedAuditMetricRows(t, s, 3, now)
	localGate(t, s)
	_, _, err := s.VerifyLocalAuditMutationState(t.Context(), model.AuditHead{}, func(model.AuditHead, model.AuditAnchor) error {
		_, err := s.CreateUpstream(t.Context(), model.UpstreamSpec{Name: "test", Kind: "forgejo", BaseURL: "https://example.test"})
		return err
	})
	if err != nil || s.localAudit.fullScans != 1 || s.localAudit.suffixes != 1 {
		t.Fatalf("benign write forced full verification: %+v err=%v", s.localAudit, err)
	}
}

func TestLocalAuditCacheFileReplacementAndRestart(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	seedAuditMetricRows(t, s, 3, now)
	localGate(t, s)
	path := s.livePath
	// Restore the original inode before cleanup: replacing a live SQLite file
	// requires reopening it, never checkpointing old WAL frames into a new file.
	if err := os.Rename(path, path+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.VerifyLocalAuditMutationState(t.Context(), model.AuditHead{}, acceptAuditMutationState)
	if restoreErr := os.Rename(path+".original", path); restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if !errors.Is(err, ErrAuditStateChanged) || s.localAudit.valid {
		t.Fatalf("replaced database reused cache: %v", err)
	}
	localGate(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestStore(t, path, &now)
	localGate(t, reopened)
	if reopened.localAudit.fullScans != 1 || reopened.localAudit.suffixes != 0 {
		t.Fatalf("restart reused a checkpoint: %+v", reopened.localAudit)
	}
}

func TestLocalAuditCacheRejectsSQLiteBackupRestore(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	seedAuditMetricRows(t, s, 3, now)
	localGate(t, s)
	snapshot := filepath.Join(t.TempDir(), "earlier.db")
	if _, err := s.db.ExecContext(t.Context(), `VACUUM INTO ?`, snapshot); err != nil {
		t.Fatal(err)
	}
	appendCacheAction(t, s)
	localGate(t, s)
	// A real SQLite restore changes the existing inode through WAL commits;
	// schema, checkpoint and prefix data all return to the earlier snapshot.
	db, err := sql.Open("sqlite", s.livePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.Raw(func(raw any) error {
		driver := raw.(interface {
			NewRestore(string) (*sqlite.Backup, error)
		})
		backup, err := driver.NewRestore(snapshot)
		if err != nil {
			return err
		}
		_, err = backup.Step(-1)
		return errors.Join(err, backup.Finish())
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.VerifyLocalAuditMutationState(t.Context(), model.AuditHead{}, acceptAuditMutationState); err == nil || !strings.Contains(err.Error(), "verified audit prefix") {
		t.Fatalf("SQLite restore discarded the prior head pin: %v", err)
	}
}

func TestLocalAuditCacheDistrustsModifiedStartupGuards(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	seedAuditMetricRows(t, s, 3, now)
	if _, err := s.db.ExecContext(t.Context(), `DROP TRIGGER audit_actions_no_update; CREATE TRIGGER audit_actions_no_update BEFORE UPDATE ON audit_actions BEGIN SELECT 1; END`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestStore(t, s.livePath, &now)
	localGate(t, reopened)
	localGate(t, reopened)
	if reopened.localAudit.valid || reopened.localAudit.fullScans != 2 {
		t.Fatal("modified startup guard became a trust root")
	}
	if _, err := reopened.db.ExecContext(t.Context(), `UPDATE audit_actions SET actor='forged' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reopened.VerifyLocalAuditMutationState(t.Context(), model.AuditHead{}, acceptAuditMutationState); err == nil || !strings.Contains(err.Error(), "audit action 1 hash is invalid") {
		t.Fatalf("modified guard hid tampering on primary connection: %v", err)
	}
}

func TestLocalAuditCacheCannotTrustTriggerReplacements(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	seedAuditMetricRows(t, s, 3, now)
	// Even a malicious trigger present before Store startup cannot use the
	// writer's trusted commits to replace an already verified audit row.
	if _, err := s.db.ExecContext(t.Context(), `CREATE TRIGGER malicious_replace AFTER INSERT ON upstreams BEGIN
INSERT OR REPLACE INTO audit_actions SELECT id, 'forged', action, resource_type, resource_id, details, previous_sha256, sha256, created_at FROM audit_actions WHERE id=1;
END`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestStore(t, s.livePath, &now)
	localGate(t, reopened)
	if !reopened.localAudit.valid {
		t.Fatal("test requires a warm trusted cache")
	}
	_, err := reopened.CreateUpstream(t.Context(), model.UpstreamSpec{Name: "test", Kind: "forgejo", BaseURL: "https://example.test"})
	if err == nil || !strings.Contains(err.Error(), "audit actions are append-only") {
		t.Fatalf("trigger replaced a verified row: %v", err)
	}
	localGate(t, reopened)
	// If a caller explicitly disables recursive triggers, reuse stops.
	if _, err := reopened.db.ExecContext(t.Context(), `PRAGMA recursive_triggers=OFF`); err != nil {
		t.Fatal(err)
	}
	localGate(t, reopened)
	if reopened.localAudit.valid || reopened.localAudit.fullScans != 2 {
		t.Fatal("cache trusted a connection with recursive triggers disabled")
	}
}

func TestLocalAuditCacheWaitHonorsCancellation(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	s.auditGate <- struct{}{}
	defer func() { <-s.auditGate }()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := s.VerifyLocalAuditMutationState(ctx, model.AuditHead{}, acceptAuditMutationState); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cache wait: %v", err)
	}
}

func BenchmarkLocalAuditGate(b *testing.B) {
	for _, rows := range []int{1000, 10000, 50000} {
		for _, mode := range []string{"cached", "append", "full"} {
			b.Run(fmt.Sprintf("%s/rows=%d", mode, rows), func(b *testing.B) {
				now := time.Now()
				s, err := OpenAdminClient(context.Background(), filepath.Join(b.TempDir(), "bench.db"), Options{})
				if err != nil {
					b.Fatal(err)
				}
				defer func() { _ = s.Close() }()
				seedAuditMetricRows(b, s, rows, now)
				localGate(b, s)
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if mode == "append" {
						appendCacheAction(b, s)
					}
					if mode == "full" {
						s.localAudit.valid = false
					}
					localGate(b, s)
				}
				b.StopTimer()
				if mode != "full" && s.localAudit.fullScans != 1 {
					b.Fatalf("benchmark unexpectedly scanned full chain: %+v", s.localAudit)
				}
			})
		}
	}
}
