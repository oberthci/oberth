package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/store"
)

func TestBackupDatabaseBrokenPipeCleansStaging(t *testing.T) {
	if path := os.Getenv("OBERTH_TEST_BROKEN_BACKUP_PIPE"); path != "" {
		os.Args = []string{"oberth", "backup", "database", "--database", path}
		main() // Real main must ignore SIGPIPE and return the write error via cleanup.
		return
	}
	t.Setenv("OBERTH_SCHEMA_IDENTITY", "sample-oberth-schema-v1")
	t.Setenv("OBERTH_AUDIT_DOMAIN", "sample-oberth-audit-v1")
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	db, err := store.CreateGenesis(t.Context(), path, store.Options{Protocol: store.ProtocolConfig{
		SchemaIdentity: "sample-oberth-schema-v1", AuditDomain: "sample-oberth-audit-v1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = reader.Close() // No reader exists before the first archive header write.
	defer func() { _ = writer.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestBackupDatabaseBrokenPipeCleansStaging$")
	cmd.Env = append(os.Environ(), "OBERTH_TEST_BROKEN_BACKUP_PIPE="+path)
	cmd.Stdout = writer
	var diagnostic bytes.Buffer
	cmd.Stderr = &diagnostic
	if err := cmd.Run(); err == nil || cmd.ProcessState.ExitCode() != 1 || !strings.Contains(diagnostic.String(), "broken pipe") {
		t.Fatal("backup must return EPIPE, not die from SIGPIPE", err, diagnostic.String())
	}
	dirs, err := filepath.Glob(filepath.Join(filepath.Dir(path), "oberth-online-backup-*"))
	if err != nil || len(dirs) != 0 {
		t.Fatal("broken stdout retained plaintext staging", dirs, err)
	}
}
