package installer

// Restore onto a reinstalled cluster (#065, #067, #068): only history and
// the operator's secrets come back; the new install keeps its own
// identities, CA and Helm values.

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// reinstallFixture backs up a source whose identities and Helm values a
// reinstalled target does not share.
func reinstallFixture(t *testing.T) (string, string, *fakeBackupCluster, *fakeBackupCluster) {
	t.Helper()
	archiveBytes, raw, runID := sourceDatabase(t)
	source := newFakeBackupCluster(t, sourceCompat, grantsConfigMap(testGrants))
	source.database, source.rawFiles = archiveBytes, raw
	source.clusterUID = "source-cluster"
	source.values["watchTunnel"] = map[string]any{"enabled": true}
	source.values["tls"] = map[string]any{"extraDNSNames": []any{"tuxbox", "localhost"}, "extraIPs": []any{"127.0.0.1"}}
	seedSourceOpenBao(source.bao)
	source.bao.kv["identities/oberth/oberth-upstream-key"] = map[string]any{"id_ed25519": "source-forge-private-canary"}
	source.bao.kv["identities/oberth/server"] = map[string]any{"ssh_host_key": "source-host-key-canary"}
	output := filepath.Join(t.TempDir(), "state.cms")
	var out bytes.Buffer
	cfg := BackupConfig{Output: output, Recipient: "recipient.pem", ToolVersion: "v0.17.34"}
	cfg.applyDefaults()
	if err := runBackup(context.Background(), cfg, source.deps(&out)); err != nil {
		t.Fatalf("backup: %v\n%s", err, out.String())
	}

	target := restoreTargetCluster(t, runID)
	target.clusterUID = "target-cluster"
	target.bao.kv["identities/oberth/oberth-upstream-key"] = map[string]any{"id_ed25519": "fresh-forge-private"}
	return output, runID, source, target
}

func runFixtureRestore(t *testing.T, archive string, target *fakeBackupCluster, cfg RestoreConfig) string {
	t.Helper()
	var out bytes.Buffer
	cfg.Input, cfg.Timeout = archive, 10*time.Second
	if !cfg.DryRun {
		cfg.Yes = true
	}
	cfg.applyDefaults()
	if err := runRestore(context.Background(), cfg, target.deps(&out)); err != nil {
		t.Fatalf("restore: %v\n%s", err, out.String())
	}
	return out.String()
}

// A backup holds the operator's secrets, not the install's own identities,
// and no Helm values.
func TestBackupLeavesOutIdentitiesAndHelmValues(t *testing.T) {
	t.Parallel()
	archive, _, _, _ := reinstallFixture(t)
	work := t.TempDir()
	manifest, err := openBackupArchive(context.Background(), archive, fakeEnvelopeCipher{}, work, 0)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(manifest.Members))
	for _, member := range manifest.Members {
		names = append(names, member.Name)
	}
	if manifest.Format != backupFormat || !slices.Equal(names, backupMemberOrder) {
		t.Fatalf("format %q members %v", manifest.Format, names)
	}
	if manifest.Source.ClusterUID != "source-cluster" {
		t.Fatalf("cluster UID = %q", manifest.Source.ClusterUID)
	}
	var kv kvExport
	if err := readBackupJSON(work, backupMemberKV, &kv); err != nil {
		t.Fatal(err)
	}
	for _, secret := range kv.Secrets {
		if strings.HasPrefix(secret.Path, installIdentityPrefix) {
			t.Fatalf("backup carries install identity %s", secret.Path)
		}
	}
}

// writeLegacyArchive writes an archive with an explicit member list, as
// older releases did.
func writeLegacyArchive(t *testing.T, dir string, manifest *backupManifest, members []string, output string) error {
	t.Helper()
	for _, name := range members {
		digest, err := digestBackupMember(filepath.Join(dir, filepath.FromSlash(name)), maxBackupCipherBytes)
		if err != nil {
			return err
		}
		digest.Name = name
		manifest.Members = append(manifest.Members, digest)
	}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	var plain bytes.Buffer
	if err := writeBackupTar(&plain, dir, body, manifest.Members); err != nil {
		return err
	}
	return writeFileForTest(output, append(fakeEnvelopeHeader(), plain.Bytes()...))
}

// v1 and v2 archives (Helm values, maybe step logs) still open; their extra
// members are verified and ignored.
func TestOpenBackupArchiveReadsOlderFormats(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		format string
		extra  []string
	}{{backupFormatV1, nil}, {backupFormatV2, nil}, {backupFormatV2, []string{backupMemberLogs}}} {
		dir := t.TempDir()
		members := append(slices.Clone(legacyMemberOrder), tc.extra...)
		for _, name := range members {
			if err := writeBackupFile(dir, name, []byte(`{}`)); err != nil {
				t.Fatal(err)
			}
		}
		manifest := backupManifest{Format: tc.format}
		output := filepath.Join(t.TempDir(), "old.cms")
		if err := writeLegacyArchive(t, dir, &manifest, members, output); err != nil {
			t.Fatal(err)
		}
		opened, err := openBackupArchive(context.Background(), output, fakeEnvelopeCipher{}, t.TempDir(), 0)
		if err != nil || opened.Format != tc.format {
			t.Fatalf("open %s %v: %+v, %v", tc.format, tc.extra, opened.Format, err)
		}
	}
}

func TestRestoreOntoAReinstalledCluster(t *testing.T) {
	t.Parallel()
	archive, _, source, target := reinstallFixture(t)
	out := runFixtureRestore(t, archive, target, RestoreConfig{})

	// Not mistaken for a rollback: both clusters serve the same API URL.
	if strings.Contains(out, "SAME cluster") || !strings.Contains(out, "a different cluster than the backup's") {
		t.Fatalf("cluster misidentified:\n%s", out)
	}
	// The install's own identities are untouched.
	if got := target.bao.kv["identities/oberth/oberth-upstream-key"]["id_ed25519"]; got != "fresh-forge-private" {
		t.Fatalf("restore replaced the install's forge key: %v", got)
	}
	if _, ok := target.bao.kv["identities/oberth/server"]; ok {
		t.Fatal("restore wrote the source's server identity")
	}
	// The operator's secrets are back.
	if canonicalJSON(target.bao.kv["release/cosign-secret"]) != canonicalJSON(source.bao.kv["release/cosign-secret"]) {
		t.Fatal("operator secret not restored")
	}
	// Only the compatibility values go onto the release.
	keys := make([]string, 0, len(target.helmValues))
	for key := range target.helmValues {
		keys = append(keys, key)
	}
	if !slices.Equal(keys, []string{"compatibility"}) {
		t.Fatalf("restore set Helm values %v, want only compatibility", keys)
	}
	// Scaling goes through the scale subresource (#067).
	for _, sub := range target.scaleSubres {
		if sub != "scale" {
			t.Fatalf("deployment patched without the scale subresource: %v", target.scaleSubres)
		}
	}
	for _, canary := range []string{"source-forge-private-canary", "source-host-key-canary", "fresh-forge-private", testSecretValue} {
		if strings.Contains(out, canary) {
			t.Fatalf("restore output prints secret material %q", canary)
		}
	}
	assertSubstringsInOrder(t, out, []string{"Unchanged  this install's identities, CA and Helm values", "Restore complete",
		"oberth install --install-secretstore --upgrade", "Re-apply your own Helm values files"})
}

func TestRestoreRecognisesTheSameCluster(t *testing.T) {
	t.Parallel()
	archive, _, _, target := reinstallFixture(t)
	target.clusterUID = "source-cluster"
	out := runFixtureRestore(t, archive, target, RestoreConfig{DryRun: true})
	if !strings.Contains(out, "SAME cluster the backup was taken from") {
		t.Fatalf("same cluster not recognised:\n%s", out)
	}
}

func TestConfirmRestoreQuotesTheNameAndRetries(t *testing.T) {
	t.Parallel()
	terminal := func() bool { return true }
	var out bytes.Buffer
	deps := backupDeps{Deps: Deps{Output: &out, Input: strings.NewReader("(default)\ntest-context\n"), IsTerminal: terminal, ContextName: "test-context"}}
	if err := confirmRestore(RestoreConfig{}, deps); err != nil {
		t.Fatalf("second answer was right: %v", err)
	}
	if !strings.Contains(out.String(), `type test-context and press Enter`) || !strings.Contains(out.String(), `"(default)" is not "test-context"`) {
		t.Fatalf("prompt = %q", out.String())
	}
	deps.Input = strings.NewReader("x\ny\nz\n")
	if err := confirmRestore(RestoreConfig{}, deps); err == nil || !strings.Contains(err.Error(), `expected "test-context"`) {
		t.Fatalf("three wrong answers: %v", err)
	}
}

// Should Helm still report a .spec.replicas conflict, the restore retries
// once with --force-conflicts instead of stopping with the server down.
func TestRestoreRetriesAHelmReplicasConflict(t *testing.T) {
	t.Parallel()
	archive, _, _, target := reinstallFixture(t)
	target.helmConflictOnce = true
	out := runFixtureRestore(t, archive, target, RestoreConfig{})
	if !slices.Contains(target.helmUpgrade, "--force-conflicts") || !strings.Contains(out, "retrying with --force-conflicts") {
		t.Fatalf("no retry: %v\n%s", target.helmUpgrade, out)
	}
}

func TestNewerArchiveFormatNamesTheRelease(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range backupMemberOrder {
		if err := writeBackupFile(dir, name, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	manifest := backupManifest{Format: "oberth.backup/v9"}
	output := filepath.Join(t.TempDir(), "v9.cms")
	if err := writeLegacyArchive(t, dir, &manifest, backupMemberOrder, output); err != nil {
		t.Fatal(err)
	}
	_, err := openBackupArchive(context.Background(), output, fakeEnvelopeCipher{}, t.TempDir(), 0)
	if err == nil || !strings.Contains(err.Error(), "use the oberth release that wrote it") {
		t.Fatalf("newer format error = %v", err)
	}
}
