//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/store"
)

func TestComposedInstallationBackupEncryptsVerifiedLiveDatabase(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	f.freshDataDirectory(t)
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	app, key := f.backupApplication(t)
	app.config.Transport = f.client(f.config.Listen.HTTPS, f.adminCertificate).Transport
	f.verifyInstallerEndpoint(t, app)
	archivePath := filepath.Join(t.TempDir(), "installation.age")
	configPath := writeBackupConfiguration(t, f)
	args := []string{"backup", "--database-only", "--config", configPath, "--out", archivePath, "--output", "json"}
	if code := app.run(t.Context(), args); code != 0 {
		t.Fatalf("backup failed: %d %s", code, app.diagnostic)
	}
	var archive installer.Archive
	if err := json.Unmarshal(app.out.(*bytes.Buffer).Bytes(), &archive); err != nil {
		t.Fatal(err)
	}
	checkComposedBackupArchive(t, archive, key)
	if archive.StoreSnapshotHint != installer.StoreSnapshotHint {
		t.Fatal("JSON omitted store snapshot hint")
	}
	f.checkBackupCompletionAudit(t)
	before, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if code := app.run(t.Context(), args); code == 0 {
		t.Fatal("backup overwrote an existing archive")
	}
	after, err := os.ReadFile(archivePath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("refused overwrite changed archive", err)
	}
	f.checkBackupTextHint(t, app, configPath)
	app.config.Transport = f.client(f.config.Listen.HTTPS, f.unknownCertificate(t)).Transport
	refused := filepath.Join(filepath.Dir(archivePath), "refused.age")
	if code := app.run(t.Context(), []string{"backup", "--database-only", "--config", configPath, "--out", refused}); code == 0 {
		t.Fatal("unknown identity received a database backup")
	}
	if _, err = os.Stat(refused); !os.IsNotExist(err) {
		t.Fatal("refused backup left an archive", err)
	}
}

func (f *composedFixture) freshDataDirectory(t *testing.T) {
	t.Helper()
	f.config.DatabaseFile = filepath.Join(t.TempDir(), "fresh-install", "state.db")
	if _, err := os.Stat(filepath.Dir(f.config.DatabaseFile)); !os.IsNotExist(err) {
		t.Fatal("installation data directory already exists", err)
	}
}

func (f *composedFixture) verifyInstallerEndpoint(t *testing.T, app application) {
	t.Helper()
	profile, err := readInstallationProfile(filepath.Join(app.base, "installed"), false)
	if err != nil {
		t.Fatal(err)
	}
	transport := f.client(f.config.Listen.HTTPS, f.adminCertificate).Transport.(*http.Transport)
	verifier := installer.EndpointVerifier{DialContext: transport.DialContext, Now: f.deps.clock.Now}
	if err = verifier.Verify(t.Context(), profile); err != nil {
		t.Fatal("real installer could not verify composed endpoint", err)
	}
}

func writeBackupConfiguration(t *testing.T, f *composedFixture) string {
	t.Helper()
	data, err := json.Marshal(f.config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "server.json")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func checkComposedBackupArchive(t *testing.T, archive installer.Archive, key *age.X25519Identity) {
	t.Helper()
	file, err := os.Open(archive.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Mode().Perm() != 0600 || archive.Report.Schema != store.Schema {
		t.Fatal("archive permissions/schema", info, archive.Report, err)
	}
	plain, err := age.Decrypt(file, key)
	if err != nil {
		t.Fatal("archive is not encrypted", err)
	}
	inspector, err := store.NewBackup(filepath.Join(t.TempDir(), "inspection.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	options, err := installationStateOptions(installationRequest{flags: installationFlags("backup")})
	if err != nil {
		t.Fatal(err)
	}
	options.Protocol.SchemaIdentity, options.Protocol.AuditDomain = "slice2-schema", "slice2-audit"
	report, err := inspector.Inspect(t.Context(), options, plain)
	if err != nil || report != archive.Report {
		t.Fatal("archive does not match verified report", report, archive.Report, err)
	}
	if _, err = io.Copy(io.Discard, plain); err != nil {
		t.Fatal(err)
	}
}

func (f *composedFixture) checkBackupTextHint(t *testing.T, app application, configPath string) {
	t.Helper()
	var output, diagnostic bytes.Buffer
	app.out, app.diagnostic = &output, &diagnostic
	path := filepath.Join(t.TempDir(), "text-backup.age")
	if code := app.run(t.Context(), []string{"backup", "--database-only", "--config", configPath, "--out", path}); code != 0 {
		t.Fatal("text backup failed", code, diagnostic.String())
	}
	text := strings.Join(strings.Fields(output.String()), " ")
	if !strings.Contains(text, strings.Join(strings.Fields(installer.StoreSnapshotHint), " ")) || !strings.Contains(text, path) {
		t.Fatal("text backup lost returned snapshot hint or path", text)
	}
}
