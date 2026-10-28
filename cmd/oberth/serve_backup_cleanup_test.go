package main

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServeSweepsInterruptedSnapshotsBeforeStartupIO(t *testing.T) {
	t.Setenv("OBERTH_IDENTITY_STORE", "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	root := t.TempDir()
	bad := filepath.Join(root, "oberth-online-backup-aaa-bad")
	good := filepath.Join(root, "oberth-online-backup-zzz-good")
	for _, dir := range []string{bad, good} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("missing", filepath.Join(bad, ".active")); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	database := filepath.Join(root, "oberth.sqlite")
	err := serve(t.Context(), serveOptions{dataRoot: root, database: database}, log.New(&logs, "", 0))
	if err == nil || !strings.Contains(err.Error(), "load in-cluster Kubernetes config") {
		t.Fatal("cleanup warning unexpectedly stopped startup", err)
	}
	if _, err := os.Stat(good); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("startup did not reclaim fresh crash staging", err)
	}
	if !strings.Contains(logs.String(), bad) {
		t.Fatal("bad entry was not reported", logs.String())
	}
	if _, err := os.Stat(database); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cleanup ran after database creation", err)
	}
}
