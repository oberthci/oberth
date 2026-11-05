package store

import (
	"bytes"
	"database/sql"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// Count actual fixture calls and genesis builds, including calls in subtests.
// The counters hold no database, clock or mutable fixture contents.
var backupFixtureCalls atomic.Int64
var backupFixtureGenesisBuilds atomic.Int64

func TestBackupFixtureReusesHistoricalGenesis(t *testing.T) {
	backupFixture(t)
	backupFixture(t)
	if builds := backupFixtureGenesisBuilds.Load(); builds != 1 {
		t.Fatalf("historical genesis builds = %d, want exactly one for independent fixture copies", builds)
	}
}

func TestBackupFixtureCopiesPreserveDurabilityAndIsolation(t *testing.T) {
	type fixture struct {
		path string
		db   *Store
		now  *time.Time
	}
	var copies []fixture
	for index := range 4 {
		path, db := backupFixture(t)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("fixture mode = %v, want regular 0600", info.Mode())
		}
		for _, prior := range copies {
			priorInfo, err := os.Stat(prior.path)
			if err != nil {
				t.Fatal(err)
			}
			if os.SameFile(info, priorInfo) || db == prior.db {
				t.Fatal("fixture copies share a file or Store")
			}
		}
		version, err := db.compatibleSchemaVersion(t.Context())
		if err != nil || version != 20 {
			t.Fatalf("fixture schema = %d, %v; want historical schema 20", version, err)
		}
		for pragma, want := range map[string]int{"synchronous": 2, "foreign_keys": 1, "wal_autocheckpoint": 0} {
			var got int
			if err := db.db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil || got != want {
				t.Fatalf("fixture %s = %d, %v; want %d", pragma, got, err, want)
			}
		}
		var journal string
		if err := db.db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil || journal != "wal" {
			t.Fatalf("fixture journal = %q, %v; want writable WAL", journal, err)
		}
		head, err := db.VerifyAuditState(t.Context())
		if err != nil || head.ID != 1 {
			t.Fatalf("private canary head = %+v, %v; want ID 1", head, err)
		}
		// An immutable reader ignores WAL. Each canary must be committed only
		// in this copy's WAL, not inherited in the closed template's main file.
		main, err := sql.Open("sqlite", currentDatabaseDSN(path, "ro", true))
		if err != nil {
			t.Fatal(err)
		}
		var tables, count int
		queryErr := main.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name='audit_actions'").Scan(&tables)
		// The original genesis fixture can keep the schema itself in WAL.
		// Either main-file layout must exclude this copy's committed canary.
		if queryErr == nil && tables != 0 {
			queryErr = main.QueryRow("SELECT count(*) FROM audit_actions").Scan(&count)
		}
		closeErr := main.Close()
		if queryErr != nil || closeErr != nil || count != 0 {
			t.Fatalf("immutable main canary count = %d, query=%v close=%v; want 0", count, queryErr, closeErr)
		}
		now := time.Date(2026, 10, 1, index, 0, 0, 0, time.UTC)
		db.now = func() time.Time { return now }
		action, err := db.AppendAuditAction(t.Context(), model.AuditActionSpec{Actor: "fixture", Action: "copy-clock", ResourceType: "backup"})
		if err != nil || action.ID != 2 || !action.CreatedAt.Equal(now) {
			t.Fatalf("private audit action = %+v, %v; want ID 2 at %v", action, err, now)
		}
		copies = append(copies, fixture{path: path, db: db, now: &now})
	}
	*copies[0].now = copies[0].now.Add(time.Hour)
	if action, err := copies[0].db.AppendAuditAction(t.Context(), model.AuditActionSpec{Actor: "fixture", Action: "advance-copy", ResourceType: "backup"}); err != nil || action.ID != 3 || !action.CreatedAt.Equal(*copies[0].now) {
		t.Fatalf("advanced private audit action = %+v, %v", action, err)
	}
	// Normal direct mutation remains refused by the real historical trigger.
	if _, err := copies[0].db.db.Exec("UPDATE audit_actions SET actor='changed-canary' WHERE id=1"); err == nil {
		t.Fatal("historical audit mutation was accepted")
	}
	// Deliberately corrupt only one copy through the same escape hatch as the
	// existing refusal case. It must emit no plaintext; all peers still verify.
	if _, err := copies[0].db.db.Exec("DROP TRIGGER audit_actions_no_update; UPDATE audit_actions SET actor='changed-canary' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	var refused bytes.Buffer
	if _, err := WriteVerifiedBackup(t.Context(), copies[0].path, 20, Options{}, &refused); err == nil || refused.Len() != 0 {
		t.Fatalf("corrupt private copy emitted plaintext: error=%v bytes=%d", err, refused.Len())
	}
	for _, copy := range copies[1:] {
		var created int64
		if err := copy.db.db.QueryRow("SELECT created_at FROM audit_actions WHERE id=2").Scan(&created); err != nil || created != copy.now.UnixNano() {
			t.Fatalf("peer clock changed: timestamp=%d, %v; want %v", created, err, *copy.now)
		}
		var out bytes.Buffer
		report, err := WriteVerifiedBackup(t.Context(), copy.path, 20, Options{}, &out)
		if err != nil || report.AuditHead.ID != 2 {
			t.Fatalf("peer backup changed after private corruption: %+v, %v", report, err)
		}
		if _, err := VerifyBackupArchive(t.Context(), bytes.NewReader(out.Bytes()), 20, Options{}); err != nil {
			t.Fatal(err)
		}
	}
	// A later copy must also be pristine after all clocks and mutations above.
	_, fresh := backupFixture(t)
	if head, err := fresh.VerifyAuditState(t.Context()); err != nil || head.ID != 1 {
		t.Fatalf("template inherited private writes: %+v, %v", head, err)
	}
}
