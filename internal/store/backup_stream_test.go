package store

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifiedBackupStreamsWithoutTempArchive(t *testing.T) {
	path, _ := backupFixture(t)
	absent := filepath.Join(t.TempDir(), "no-system-temporary-storage")
	t.Setenv("TMPDIR", absent)
	stream, err := OpenVerifiedBackup(t.Context(), path, 20, Options{})
	if err != nil {
		t.Fatal("backup depends on system temporary storage", err)
	}
	var archive bytes.Buffer
	if _, err := io.Copy(&archive, stream); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	verifyDir := t.TempDir()
	if _, err := VerifyBackupArchive(t.Context(), &archive, 20, Options{SnapshotTempDir: verifyDir}); err != nil {
		t.Fatal("stream not verified", err)
	}
	if _, err := os.Stat(absent); !os.IsNotExist(err) {
		t.Fatal("backup staged a second archive", err)
	}
	for _, file := range []string{path} {
		entries, err := os.ReadDir(filepath.Dir(file))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				t.Fatal("snapshot not cleaned", entry.Name())
			}
		}
	}
	stream, err = OpenVerifiedBackup(t.Context(), path, 20, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal("unread stream could not close", err)
	}
}
