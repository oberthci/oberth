package store

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func backupFixture(t *testing.T) (string, *Store) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "source.sqlite")
	db, e := CreateGenesis(context.Background(), p, Options{})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, e := db.db.Exec("DROP TABLE IF EXISTS step_resource_usage; DROP INDEX IF EXISTS runs_latest_ref_idx; DROP TABLE IF EXISTS run_priority_overrides; DROP TABLE IF EXISTS queue_priority_patterns; DELETE FROM schema_migrations WHERE version>=34; ALTER TABLE step_results DROP COLUMN memory_peak_bytes; DELETE FROM schema_migrations WHERE version=33; ALTER TABLE schedule_fires DROP COLUMN last_fired_at; DELETE FROM schema_migrations WHERE version=32; DROP TRIGGER run_schedule_entry_immutable; ALTER TABLE runs DROP COLUMN schedule_entry; DELETE FROM schema_migrations WHERE version=31; DROP TRIGGER run_timing_claim; DROP TRIGGER run_timing_grant; DROP TRIGGER run_timing_park; DROP TRIGGER run_timing_terminal; DROP TABLE step_timings; DROP TABLE run_admission_timings; DROP TABLE run_timings; DROP INDEX runs_admission_idx; ALTER TABLE runs DROP COLUMN admission_sequence; DROP TABLE kubevirt_grants; DROP TABLE uplink_scopes; ALTER TABLE issues DROP COLUMN scope_upstream_id; ALTER TABLE step_results DROP COLUMN termination_json; DELETE FROM schema_migrations WHERE version>=21; PRAGMA wal_autocheckpoint=0"); e != nil {
		t.Fatal(e)
	}
	if _, e := db.AppendAuditAction(context.Background(), model.AuditActionSpec{Actor: "fixture", Action: "backup-test", ResourceType: "backup", ResourceID: "private-canary"}); e != nil {
		t.Fatal(e)
	}
	return p, db
}
func backupSourceBytes(t *testing.T, p string) map[string][]byte {
	t.Helper()
	r := map[string][]byte{}
	for _, s := range []string{"", "-wal", "-shm"} {
		b, e := os.ReadFile(p + s)
		if e == nil {
			r[s] = b
		} else if !errors.Is(e, os.ErrNotExist) {
			t.Fatal(e)
		}
	}
	return r
}
func TestVerifiedBackupFoldsCommittedWALAndPreservesSource(t *testing.T) {
	p, _ := backupFixture(t)
	before := backupSourceBytes(t, p)
	var out bytes.Buffer
	report, e := WriteVerifiedBackup(context.Background(), p, 20, Options{}, &out)
	if e != nil {
		t.Fatal(e)
	}
	if report.Schema != 20 || !report.Integrity || !report.ForeignKeys || report.AuditHead.ID != 1 {
		t.Fatalf("unexpected public report: %+v", report)
	}
	after := backupSourceBytes(t, p)
	if len(before) != len(after) {
		t.Fatal("source file set changed")
	}
	for k, v := range before {
		if k != "-shm" && !bytes.Equal(v, after[k]) {
			t.Fatalf("source changed %q", k)
		}
	}
	dir := t.TempDir()
	reader := tar.NewReader(bytes.NewReader(out.Bytes()))
	seen := map[string]bool{}
	for {
		h, e := reader.Next()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		if seen[h.Name] || h.Mode != 0600 || h.Typeflag != tar.TypeReg {
			t.Fatal("archive custody refused")
		}
		seen[h.Name] = true
		if h.Name != "oberth.sqlite" && h.Name != "oberth.sqlite-wal" && h.Name != "manifest.json" {
			t.Fatal("unexpected archive entry")
		}
		b, e := io.ReadAll(reader)
		if e != nil {
			t.Fatal(e)
		}
		if h.Name == "manifest.json" {
			var m backupManifest
			if e := json.Unmarshal(b, &m); e != nil || m.Schema != 20 || len(m.Members) != 1 {
				t.Fatal("manifest refused")
			}
			continue
		}
		if e := os.WriteFile(filepath.Join(dir, h.Name), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	if len(seen) != 2 || seen["oberth.sqlite-wal"] {
		t.Fatal("online archive must fold WAL into one main file")
	}
	inspection, e := InspectCurrent(context.Background(), filepath.Join(dir, "oberth.sqlite"), Options{})
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = inspection.Close() }()
	version, e := inspection.compatibleSchemaVersion(context.Background())
	if e != nil || version != 20 {
		t.Fatal("inspection migrated snapshot")
	}
	head, e := inspection.VerifyAuditState(context.Background())
	if e != nil || head.ID != 1 {
		t.Fatal("WAL committed audit missing")
	}
}
func TestVerifiedBackupRefusesBeforeOutput(t *testing.T) {
	for _, name := range []string{"wrong-schema", "cancelled", "corrupt-audit", "symlink", "oversize"} {
		t.Run(name, func(t *testing.T) {
			p, db := backupFixture(t)
			ctx := context.Background()
			want := 20
			switch name {
			case "wrong-schema":
				want = 21
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "corrupt-audit":
				if _, e := db.db.Exec("DROP TRIGGER audit_actions_no_update; UPDATE audit_actions SET actor='changed-canary' WHERE id=1"); e != nil {
					t.Fatal(e)
				}
			case "symlink":
				q := filepath.Join(t.TempDir(), "alias")
				if e := os.Symlink(p, q); e != nil {
					t.Fatal(e)
				}
				p = q
			case "oversize":
				q := filepath.Join(t.TempDir(), "huge")
				f, e := os.Create(q)
				if e != nil {
					t.Fatal(e)
				}
				if e := f.Truncate(129 << 20); e != nil {
					t.Fatal(e)
				}
				_ = f.Close()
				p = q
			}
			var out bytes.Buffer
			if _, e := WriteVerifiedBackup(ctx, p, want, Options{}, &out); e == nil || out.Len() != 0 {
				t.Fatal("invalid snapshot emitted plaintext")
			}
		})
	}
}

type backupRefusingWriter struct{}

func (backupRefusingWriter) Write([]byte) (int, error) {
	return 0, errors.New("fixture output refused")
}
func TestVerifiedBackupPropagatesWriterFailure(t *testing.T) {
	p, _ := backupFixture(t)
	if _, e := WriteVerifiedBackup(context.Background(), p, 20, Options{}, backupRefusingWriter{}); e == nil {
		t.Fatal("writer failure accepted")
	}
}

func TestVerifiedBackupArchiveRejectsChangedInventory(t *testing.T) {
	p, _ := backupFixture(t)
	var out bytes.Buffer
	if _, e := WriteVerifiedBackup(context.Background(), p, 20, Options{}, &out); e != nil {
		t.Fatal(e)
	}
	if _, e := VerifyBackupArchive(context.Background(), bytes.NewReader(out.Bytes()), 20, Options{}); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"trailing", "truncated", "wrong-schema", "bad-name", "duplicate", "hash"} {
		t.Run(name, func(t *testing.T) {
			raw := append([]byte(nil), out.Bytes()...)
			expected := 20
			switch name {
			case "trailing":
				raw = append(raw, 1)
			case "truncated":
				raw = raw[:len(raw)-512]
			case "wrong-schema":
				expected = 21
			default:
				source := tar.NewReader(bytes.NewReader(raw))
				var rebuilt bytes.Buffer
				writer := tar.NewWriter(&rebuilt)
				for {
					h, e := source.Next()
					if errors.Is(e, io.EOF) {
						break
					}
					if e != nil {
						t.Fatal(e)
					}
					b, e := io.ReadAll(source)
					if e != nil {
						t.Fatal(e)
					}
					if name == "bad-name" && h.Name == "oberth.sqlite" {
						h.Name = "../escape"
					}
					if name == "hash" && h.Name == "manifest.json" {
						b = bytes.Replace(b, []byte(`"sha256":"`), []byte(`"sha256":"0`), 1)
						h.Size = int64(len(b))
					}
					if e := writer.WriteHeader(h); e != nil {
						t.Fatal(e)
					}
					if _, e := writer.Write(b); e != nil {
						t.Fatal(e)
					}
					if name == "duplicate" && h.Name == "manifest.json" {
						if e := writer.WriteHeader(h); e != nil {
							t.Fatal(e)
						}
						if _, e := writer.Write(b); e != nil {
							t.Fatal(e)
						}
					}
				}
				if e := writer.Close(); e != nil {
					t.Fatal(e)
				}
				raw = rebuilt.Bytes()
			}
			if _, e := VerifyBackupArchive(context.Background(), bytes.NewReader(raw), expected, Options{}); e == nil {
				t.Fatal("invalid archive accepted")
			}
		})
	}
}

func TestVerifiedBackupConcurrentWriterCheckpoint(t *testing.T) {
	p, db := backupFixture(t)
	ctx := context.Background()
	start := make(chan struct{})
	stop := make(chan struct{})
	joined := make(chan error, 1)
	go func() {
		close(start)
		for i := 0; i < 64; i++ {
			select {
			case <-stop:
				joined <- nil
				return
			default:
			}
			if _, e := db.AppendAuditAction(ctx, model.AuditActionSpec{Actor: "writer", Action: "backup-concurrency", ResourceType: "fixture", ResourceID: fmt.Sprint(i)}); e != nil {
				joined <- e
				return
			}
			if _, e := db.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); e != nil {
				joined <- e
				return
			}
		}
		joined <- nil
	}()
	<-start
	for i := 0; i < 4; i++ {
		var out bytes.Buffer
		_, e := WriteVerifiedBackup(ctx, p, 20, Options{}, &out)
		if e != nil {
			close(stop)
			<-joined
			t.Fatalf("online backup must succeed during writers/checkpoints: %v", e)
		}
		if _, e := VerifyBackupArchive(ctx, bytes.NewReader(out.Bytes()), 20, Options{}); e != nil {
			close(stop)
			<-joined
			t.Fatal("successful concurrent snapshot was not coherent")
		}
	}
	close(stop)
	if e := <-joined; e != nil {
		t.Fatal(e)
	}
	// The stable follow-up is a different synthetic assertion, not a production
	// retry policy. All production calls above either refused or verified.
	var stable bytes.Buffer
	if _, e := WriteVerifiedBackup(ctx, p, 20, Options{}, &stable); e != nil {
		t.Fatal(e)
	}
	if _, e := VerifyBackupArchive(ctx, bytes.NewReader(stable.Bytes()), 20, Options{}); e != nil {
		t.Fatal(e)
	}
}
