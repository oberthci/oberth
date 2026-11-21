package installer_test

import (
	"bytes"

	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/stateport"
	statefake "github.com/oberthci/oberth/internal/stateport/fake"
)

func backupFixture(t *testing.T) (*statefake.Backup, stateport.Options) {
	t.Helper()
	options := stateport.Options{Protocol: stateport.DefaultProtocol(), Schema: 1, MaxBytes: 1 << 20}
	report := stateport.BackupReport{Schema: 1, AuditHead: stateport.AuditHead{ID: 7, SHA256: strings.Repeat("a", 64)}, Integrity: true, ForeignKeys: true}
	source, err := statefake.New(options.Protocol, report, []byte("canonical verified seed"))
	if err != nil {
		t.Fatal(err)
	}
	return source, options
}
// Covers: REQ-C9-004
func TestBackupIsAgeAndDryRunCreatesNoFiles(t *testing.T) {
	t.Parallel()
	source, options := backupFixture(t)
	key, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	cfg := installer.BackupConfig{Source: source, Options: options, Recipient: key.Recipient().String(), Path: filepath.Join(t.TempDir(), "backup.age"), DryRun: true}
	if _, err = installer.Backup(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(cfg.Path); !os.IsNotExist(err) {
		t.Fatal("dry run wrote backup")
	}
	cfg.DryRun = false
	archive, err := installer.Backup(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	decrypted, err := age.Decrypt(bytes.NewReader(raw), key)
	if err != nil {
		t.Fatal("not native age", err)
	}
	report, err := source.Inspect(t.Context(), options, decrypted)
	if err != nil || report != archive.Report {
		t.Fatal("archive is not canonical stateport", err)
	}
	st, err := os.Stat(cfg.Path)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("public backup", err)
	}
}
