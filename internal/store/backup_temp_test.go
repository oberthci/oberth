package store

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

type backupTempObserver struct {
	io.Reader
	check func()
}

func (r backupTempObserver) Read(p []byte) (int, error) { r.check(); return r.Reader.Read(p) }

func TestBackupVerificationAndInspectionHonorTMPDIR(t *testing.T) {
	path, _ := backupFixture(t)
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", root)
	var archive bytes.Buffer
	if _, err := WriteVerifiedBackup(t.Context(), path, 20, Options{}, &archive); err != nil {
		t.Fatal(err)
	}
	seen := false
	reader := backupTempObserver{Reader: &archive, check: func() {
		dirs, err := filepath.Glob(filepath.Join(root, "oberth-backup-verify-*"))
		if err != nil {
			t.Fatal(err)
		}
		seen = seen || len(dirs) == 1
	}}
	if _, err := VerifyBackupArchive(t.Context(), reader, 20, Options{}); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("verification ignored TMPDIR")
	}
	inspection, err := InspectCurrent(t.Context(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inspection.Close() })
	var seq int
	var name, database string
	if err := inspection.db.QueryRow("PRAGMA database_list").Scan(&seq, &name, &database); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(database, root+string(filepath.Separator)) {
		t.Fatal("inspection ignored TMPDIR", database)
	}
	if err := inspection.Close(); err != nil {
		t.Fatal(err)
	}
	dirs, err := filepath.Glob(filepath.Join(root, "oberth-*"))
	if err != nil || len(dirs) != 0 {
		t.Fatal("plaintext verification directories retained", dirs, err)
	}
	// Explicit callers (including the maintenance sandbox) can pin a private
	// directory independently of ambient TMPDIR.
	t.Setenv("TMPDIR", filepath.Join(root, "does-not-exist"))
	if _, err := WriteVerifiedBackup(t.Context(), path, 20, Options{}, &archive); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBackupArchive(t.Context(), &archive, 20, Options{SnapshotTempDir: root}); err != nil {
		t.Fatal("explicit verification staging ignored", err)
	}
	inspection, err = InspectCurrent(t.Context(), path, Options{SnapshotTempDir: root})
	if err != nil {
		t.Fatal("explicit inspection staging ignored", err)
	}
	if err := inspection.Close(); err != nil {
		t.Fatal(err)
	}
}
