package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// isolatedHome points HOME, XDG_CONFIG_HOME and KUBECONFIG at a fresh
// directory, so default key and archive paths never resolve to the
// operator's own and no test can reach a real cluster.
func isolatedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("KUBECONFIG", filepath.Join(home, "no-kubeconfig"))
	t.Setenv("BAO_TOKEN", "")
	return home
}

// The first backup on a machine creates the key itself (#065): no keygen
// step, no --recipient.
func TestFirstBackupCreatesTheKey(t *testing.T) {
	home := isolatedHome(t)
	var out bytes.Buffer
	err := runCLI(context.Background(), []string{"backup"}, strings.NewReader(""), &out)
	// No cluster here (and on CI no kubectl either): the backup itself
	// fails, after creating the key.
	if err == nil {
		t.Fatal("backup without a cluster succeeded")
	}
	key := filepath.Join(home, ".config", "oberth", "backup", "recipient.key")
	if info, statErr := os.Stat(key); statErr != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("first backup did not create %s (0600): %v", key, statErr)
	}
	if !strings.Contains(out.String(), "Created the backup key") || !strings.Contains(out.String(), "OFF this machine") {
		t.Fatalf("output does not announce the new key:\n%s", out.String())
	}
	// A dry run on a machine without a key says it would create one.
	other := isolatedHome(t)
	out.Reset()
	_ = runCLI(context.Background(), []string{"backup", "--dry-run"}, strings.NewReader(""), &out)
	if _, statErr := os.Stat(filepath.Join(other, ".config", "oberth", "backup", "recipient.key")); statErr == nil {
		t.Fatal("a dry run created the key")
	}
	if !strings.Contains(out.String(), "a real backup creates") {
		t.Fatalf("dry run output = %s", out.String())
	}
}

func TestRestoreFindsTheBackupAndKey(t *testing.T) {
	home := isolatedHome(t)
	err := runCLI(context.Background(), []string{"restore"}, strings.NewReader(""), &bytes.Buffer{})
	if !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "no oberth backup") {
		t.Fatalf("restore with no backup around = %v", err)
	}
	dir := filepath.Join(home, "carried")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	older, newer := filepath.Join(dir, "oberth-backup-1.tar.gz.enc"), filepath.Join(dir, "oberth-backup-2.tar.gz.enc")
	for i, path := range []string{older, newer} {
		if err := os.WriteFile(path, []byte("not a real archive"), 0o600); err != nil {
			t.Fatal(err)
		}
		stamp := time.Now().Add(time.Duration(i-2) * time.Hour)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	err = runCLI(context.Background(), []string{"restore", dir, "--inspect"}, strings.NewReader(""), &out)
	if !strings.Contains(out.String(), "Using oberth-backup-2.tar.gz.enc, the newest backup in") {
		t.Fatalf("directory not resolved to its newest backup:\n%s", out.String())
	}
	if err == nil || !strings.Contains(err.Error(), "no backup key") {
		t.Fatalf("restore without a key = %v, want the key guidance", err)
	}
	// A key carried next to the backup is used.
	if err := os.WriteFile(filepath.Join(dir, "recipient.key"), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = runCLI(context.Background(), []string{"restore", "--inspect", newer}, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || strings.Contains(err.Error(), "no backup key") || !strings.Contains(err.Error(), "AuthEnvelopedData") {
		t.Fatalf("restore with the key beside the backup = %v, want it to reach the archive check", err)
	}
}

func TestParseOperatorFlagsTakesFlagsAfterArguments(t *testing.T) {
	t.Parallel()
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dryRun := flags.Bool("dry-run", false, "")
	only := flags.String("only", "", "")
	positional, help, err := parseOperatorFlags(flags, []string{"backup.tar.gz.enc", "--dry-run", "--only", "history"})
	if err != nil || help || !*dryRun || *only != "history" || len(positional) != 1 || positional[0] != "backup.tar.gz.enc" {
		t.Fatalf("positional %v help %v err %v dry-run %v only %q", positional, help, err, *dryRun, *only)
	}
}

// The transcript of #065: --help after a flag that takes a value showed a
// --recipient error, and errors spelled flags with one dash.
func TestBackupFlagsBehaveForOperators(t *testing.T) {
	isolatedHome(t)
	var out bytes.Buffer
	if err := runCLI(context.Background(), []string{"backup", "--output", "--help"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("--output --help: %v", err)
	}
	if !strings.Contains(out.String(), "Usage: oberth backup") || strings.Contains(out.String(), "  -output") {
		t.Fatalf("help = %s", out.String())
	}
	err := runCLI(context.Background(), []string{"backup", "--output"}, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--output") || strings.Contains(err.Error(), " -output") {
		t.Fatalf("missing value error = %v", err)
	}
	err = runCLI(context.Background(), []string{"backup", "--output", "--history-only"}, strings.NewReader(""), &bytes.Buffer{})
	if !errors.Is(err, errUsage) || !strings.Contains(err.Error(), `--output needs a value, but got "--history-only"`) {
		t.Fatalf("flag-as-value error = %v", err)
	}
	err = runCLI(context.Background(), []string{"backup", "extra"}, strings.NewReader(""), &bytes.Buffer{})
	if !errors.Is(err, errUsage) {
		t.Fatalf("positional argument error = %v, want usage error", err)
	}
}

func TestBackupKeygenCreatesAUsableRecipientOnce(t *testing.T) {
	home := isolatedHome(t)
	var out bytes.Buffer
	if err := runCLI(context.Background(), []string{"backup", "keygen"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(home, ".config", "oberth", "backup", "recipient.key")
	info, err := os.Stat(key)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private key %v mode %v", err, info)
	}
	if !strings.Contains(out.String(), "OFF this machine") {
		t.Fatalf("keygen does not say to copy the key away:\n%s", out.String())
	}
	if err := runCLI(context.Background(), []string{"backup", "keygen"}, strings.NewReader(""), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "never replaced") {
		t.Fatalf("second keygen = %v, want a refusal", err)
	}
}

func TestDoubleDashFlags(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"flag needs an argument: -output":                         "flag needs an argument: --output",
		"flag provided but not defined: -limit":                   "flag provided but not defined: --limit",
		`invalid value "x" for flag -timeout: parse error`:        `invalid value "x" for flag --timeout: parse error`,
		"already --double and a-hyphenated-word stay as they are": "already --double and a-hyphenated-word stay as they are",
	} {
		if got := doubleDashFlags(in); got != want {
			t.Errorf("doubleDashFlags(%q) = %q, want %q", in, got, want)
		}
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

func TestBackupRefusesACertificateWithoutItsKey(t *testing.T) {
	home := isolatedHome(t)
	if err := runCLI(context.Background(), []string{"backup", "keygen"}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(home, ".config", "oberth", "backup", "recipient.key")); err != nil {
		t.Fatal(err)
	}
	err := runCLI(context.Background(), []string{"backup"}, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "could never be restored") {
		t.Fatalf("backup to a key-less certificate = %v", err)
	}
}

func TestRestoreYesNeedsAnExplicitBackup(t *testing.T) {
	isolatedHome(t)
	err := runCLI(context.Background(), []string{"restore", "--yes"}, strings.NewReader(""), &bytes.Buffer{})
	if !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "name the backup") {
		t.Fatalf("restore --yes without a file = %v", err)
	}
}
