package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func TestHelpListsBackupAndRestore(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := runCLI(context.Background(), []string{"--help"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"  backup        ", "  restore       "} {
		if !strings.Contains(out.String(), line) {
			t.Fatalf("help lacks %q:\n%s", line, out.String())
		}
	}
	if !strings.Contains(usageCommands, "backup, restore") {
		t.Fatalf("usage summary lacks backup and restore: %s", usageCommands)
	}
}

func TestBackupAndRestoreRequireTheirFiles(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"backup", "--recipient", "cert.pem"}, "--output is required"},
		{[]string{"backup", "--output", "state.cms"}, "--recipient is required"},
		{[]string{"restore", "--private-key", "key.pem"}, "--input is required"},
		{[]string{"restore", "--input", "state.cms"}, "--private-key is required"},
	} {
		err := runCLI(context.Background(), tc.args, strings.NewReader(""), &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%v: error = %v, want %q", tc.args, err, tc.want)
		}
	}
	err := runCLI(context.Background(), []string{"backup", "extra"}, strings.NewReader(""), &bytes.Buffer{})
	if !errors.Is(err, errUsage) {
		t.Fatalf("positional argument error = %v, want usage error", err)
	}
}

// TestBackupDatabaseWritesAVerifiedArchive runs the in-pod half under the
// deployment's protocol environment and verifies its output offline.
func TestBackupDatabaseWritesAVerifiedArchive(t *testing.T) {
	t.Setenv("OBERTH_SCHEMA_IDENTITY", "sample-oberth-schema-v1")
	t.Setenv("OBERTH_AUDIT_DOMAIN", "sample-oberth-audit-v1")
	protocol := store.ProtocolConfig{SchemaIdentity: "sample-oberth-schema-v1", AuditDomain: "sample-oberth-audit-v1"}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := store.CreateGenesis(ctx, path, store.Options{Protocol: protocol})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.AppendAuditAction(ctx, model.AuditActionSpec{Actor: "test", Action: "backup", ResourceType: "backup", ResourceID: "cli"}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	if err := runCLI(ctx, []string{"backup", "database", "--database", path}, strings.NewReader(""), &archive); err != nil {
		t.Fatal(err)
	}
	report, err := store.VerifyBackupArchive(ctx, &archive, store.LatestMigrationVersion(), store.Options{Protocol: protocol})
	if err != nil {
		t.Fatal(err)
	}
	if report.AuditHead.ID != 1 || !report.Integrity || !report.ForeignKeys {
		t.Fatalf("report = %+v", report)
	}
}
