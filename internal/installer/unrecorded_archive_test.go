package installer

import (
	"os"
	"testing"
)

func TestDiscardUnrecordedArchiveLeavesIncompleteFiles(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"private", "public", "absent"} {
		t.Run(kind, func(t *testing.T) { discardUnrecordedCase(t, kind) })
	}
}

func discardUnrecordedCase(t *testing.T, kind string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	credentials, err := OpenConfigDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = credentials.Close() }()
	path := credentials.pathFor("backup-unrecorded.age")
	orphan := credentials.pathFor(".oberth-backup-incomplete-unrelated")
	if err = os.WriteFile(orphan, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if kind != "absent" {
		mode := os.FileMode(0600)
		if kind == "public" {
			mode = 0644
		}
		if err = os.WriteFile(path, []byte("complete unrecorded archive"), mode); err != nil {
			t.Fatal(err)
		}
	}
	m := Maintenance{Engine: &Engine{Credentials: credentials}}
	if err = m.discardUnrecorded(path); (err != nil) != (kind == "public") {
		t.Fatal("unexpected unrecorded-archive cleanup", err)
	}
	if _, err = os.Stat(path); (err == nil) != (kind == "public") {
		t.Fatal("cleanup changed an unowned archive or retained its own", err)
	}
	if raw, err := os.ReadFile(orphan); err != nil || string(raw) != "keep" {
		t.Fatal("unrecorded cleanup changed an incomplete file", err)
	}
}
