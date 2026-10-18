package installer

// Restore onto a reinstalled cluster (#065, #067, #068).

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// testSSHKey returns an authorized_keys line and an OpenSSH private key PEM.
func testSSHKey(t *testing.T) (string, string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPublic, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPublic))), string(pem.EncodeToMemory(block))
}

// reinstallFixture backs up a source whose forge key, host key, tunnel and
// certificate names a reinstalled target does not share.
func reinstallFixture(t *testing.T) (string, string, *fakeBackupCluster, *fakeBackupCluster) {
	t.Helper()
	sourceForge, _ := testSSHKey(t)
	_, sourceHost := testSSHKey(t)
	archiveBytes, raw, runID := sourceDatabase(t)
	source := newFakeBackupCluster(t, sourceCompat, grantsConfigMap(testGrants))
	source.database, source.rawFiles = archiveBytes, raw
	source.logsTar = logsTarFixture(t, map[string]string{"logs/" + runID + ".log": "step output\n", "logs/" + runID + ".index.json": "{}"})
	source.clusterUID, source.jwksKeyIDs, source.signingKeyID = "source-cluster", []string{"old-kid"}, "old-kid"
	source.values["argo"] = map[string]any{"namespace": DefaultArgoNamespace, "releaseWIF": map[string]any{"namespace": DefaultArgoNamespace}}
	source.values["watchTunnel"] = map[string]any{"enabled": true, "originCACert": "-----BEGIN CERTIFICATE-----\nold-ca\n-----END CERTIFICATE-----\n"}
	source.values["tls"] = map[string]any{"extraDNSNames": []any{"tuxbox", "localhost"}, "extraIPs": []any{"192.168.86.193", "127.0.0.1"}}
	seedSourceOpenBao(source.bao)
	source.bao.kv[forgeKeyKVPath(DefaultNamespace)] = map[string]any{"id_ed25519.pub": sourceForge, "id_ed25519": "forge-private-canary"}
	source.bao.kv[serverIdentityKVPath(DefaultNamespace)] = map[string]any{"ssh_host_key": sourceHost}
	output := filepath.Join(t.TempDir(), "state.cms")
	var out bytes.Buffer
	cfg := BackupConfig{Output: output, Recipient: "recipient.pem", ToolVersion: "v0.17.32"}
	cfg.applyDefaults()
	if err := runBackup(context.Background(), cfg, source.deps(&out)); err != nil {
		t.Fatalf("backup: %v\n%s", err, out.String())
	}

	targetForge, _ := testSSHKey(t)
	_, targetHost := testSSHKey(t)
	target := restoreTargetCluster(t, runID)
	target.clusterUID, target.jwksKeyIDs, target.signingKeyID = "target-cluster", []string{"new-kid"}, "new-kid"
	target.values["tls"] = map[string]any{"extraDNSNames": []any{"tuxbox", "tuxbox.local"}, "extraIPs": []any{"192.168.86.193"}}
	target.bao.kv[forgeKeyKVPath(DefaultNamespace)] = map[string]any{"id_ed25519.pub": targetForge, "id_ed25519": "fresh-forge-private"}
	target.bao.kv[serverIdentityKVPath(DefaultNamespace)] = map[string]any{"ssh_host_key": targetHost}
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

func TestBackupRecordsStepLogsAndClusterIdentity(t *testing.T) {
	t.Parallel()
	archive, _, _, _ := reinstallFixture(t)
	manifest, err := openBackupArchive(context.Background(), archive, fakeEnvelopeCipher{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Format != backupFormat || manifest.Logs == nil || manifest.Logs.Files != 2 {
		t.Fatalf("format %q logs %+v, want %s with 2 log files", manifest.Format, manifest.Logs, backupFormat)
	}
	if last := manifest.Members[len(manifest.Members)-1]; last.Name != backupMemberLogs {
		t.Fatalf("last member = %s, want %s", last.Name, backupMemberLogs)
	}
	if manifest.Source.ClusterUID != "source-cluster" || manifest.Source.SigningKeyID != "old-kid" || !slices.Equal(manifest.Source.ServiceAccountKeyIDs, []string{"old-kid"}) {
		t.Fatalf("source identity = %+v", manifest.Source)
	}
}

func TestBackupWithoutLogs(t *testing.T) {
	t.Parallel()
	_, _, source := backupFixtureArchive(t, false)
	output := filepath.Join(t.TempDir(), "state.cms")
	var out bytes.Buffer
	cfg := BackupConfig{Output: output, Recipient: "recipient.pem", NoLogs: true}
	cfg.applyDefaults()
	if err := runBackup(context.Background(), cfg, source.deps(&out)); err != nil {
		t.Fatal(err)
	}
	manifest, err := openBackupArchive(context.Background(), output, fakeEnvelopeCipher{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Logs != nil || len(manifest.Members) != len(backupMemberOrder) {
		t.Fatalf("--no-logs archive has logs: %+v", manifest.Members)
	}
	if !strings.Contains(out.String(), "left out (--no-logs)") {
		t.Fatalf("output does not say logs were left out:\n%s", out.String())
	}
}

// A v1 archive (no optional members) written by v0.17.31 still opens.
func TestOpenBackupArchiveReadsV1(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range backupMemberOrder {
		if err := writeBackupFile(dir, name, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	manifest := backupManifest{Format: backupFormatV1}
	output := filepath.Join(t.TempDir(), "v1.cms")
	if err := writeBackupArchive(context.Background(), dir, &manifest, fakeEnvelopeCipher{}, output); err != nil {
		t.Fatal(err)
	}
	opened, err := openBackupArchive(context.Background(), output, fakeEnvelopeCipher{}, t.TempDir())
	if err != nil || opened.Format != backupFormatV1 {
		t.Fatalf("open v1 archive: %+v, %v", opened.Format, err)
	}
}

func TestValidateLogsTarAdmitsOnlyLogFiles(t *testing.T) {
	t.Parallel()
	if _, err := validateLogsTar(logsTarFixture(t, map[string]string{"logs/run.log": "x"})); err != nil {
		t.Fatalf("plain logs refused: %v", err)
	}
	for _, name := range []string{"logs/../oberth.sqlite", "oberth.sqlite", "logs/sub/run.log", "logs/.hidden", "/logs/run.log"} {
		if _, err := validateLogsTar(logsTarFixture(t, map[string]string{name: "x"})); err == nil {
			t.Errorf("entry %q was admitted", name)
		}
	}
}

func TestRestoreOntoAReinstalledCluster(t *testing.T) {
	t.Parallel()
	archive, _, source, target := reinstallFixture(t)
	freshForge := target.bao.kv[forgeKeyKVPath(DefaultNamespace)]["id_ed25519.pub"]
	out := runFixtureRestore(t, archive, target, RestoreConfig{})

	// Not mistaken for a rollback: both clusters serve the same API URL.
	if strings.Contains(out, "SAME cluster") || !strings.Contains(out, "a different cluster than the backup's") {
		t.Fatalf("cluster misidentified:\n%s", out)
	}
	// The forge key the fresh install registered is kept.
	if got := target.bao.kv[forgeKeyKVPath(DefaultNamespace)]["id_ed25519.pub"]; got != freshForge {
		t.Fatal("restore replaced the target's forge key")
	}
	if !strings.Contains(out, "keeping this install's key") {
		t.Fatalf("plan does not explain the kept forge key:\n%s", out)
	}
	// The host key returns to the archive's, and the plan says so.
	if canonicalJSON(target.bao.kv[serverIdentityKVPath(DefaultNamespace)]) != canonicalJSON(source.bao.kv[serverIdentityKVPath(DefaultNamespace)]) {
		t.Fatal("server identity not restored")
	}
	if !strings.Contains(out, "host key returns to the archive's") || !strings.Contains(out, "ssh-keygen -R") {
		t.Fatalf("host key change not explained:\n%s", out)
	}
	// The tunnel is re-enabled against this install's CA, and the source
	// certificate names are added to the target's.
	tunnel, _ := target.helmValues["watchTunnel"].(map[string]any)
	if tunnel["enabled"] != true || !strings.Contains(tunnel["originCACert"].(string), "old-ca") {
		t.Fatalf("watchTunnel overlay = %v", target.helmValues["watchTunnel"])
	}
	tls, _ := target.helmValues["tls"].(map[string]any)
	if !slices.Equal(anyStrings(tls["extraDNSNames"]), []string{"tuxbox", "tuxbox.local", "localhost"}) ||
		!slices.Equal(anyStrings(tls["extraIPs"]), []string{"192.168.86.193", "127.0.0.1"}) {
		t.Fatalf("tls overlay = %v", target.helmValues["tls"])
	}
	// Step logs went back through the helper pod, hash-checked on the volume.
	if target.logsRestored == "" || len(target.uploads["logs.tar"]) == 0 || !strings.Contains(out, "step logs restored") {
		t.Fatalf("step logs not restored (sha %q):\n%s", target.logsRestored, out)
	}
	// The new service-account key is called out for release WIF.
	if !strings.Contains(out, "trusts the source cluster's service-account key old-kid, but this cluster signs with new-kid") {
		t.Fatalf("service-account key change not noted:\n%s", out)
	}
	// Scaling goes through the scale subresource (#067).
	for _, sub := range target.scaleSubres {
		if sub != "scale" {
			t.Fatalf("deployment patched without the scale subresource: %v", target.scaleSubres)
		}
	}
	if strings.Contains(out, "forge-private-canary") || strings.Contains(out, "fresh-forge-private") {
		t.Fatal("restore output prints a private key")
	}
}

func TestRestoreForgeKeyFlagRestoresTheArchivedKey(t *testing.T) {
	t.Parallel()
	archive, _, source, target := reinstallFixture(t)
	out := runFixtureRestore(t, archive, target, RestoreConfig{RestoreForgeKey: true})
	if canonicalJSON(target.bao.kv[forgeKeyKVPath(DefaultNamespace)]) != canonicalJSON(source.bao.kv[forgeKeyKVPath(DefaultNamespace)]) {
		t.Fatal("--restore-forge-key did not restore the archived forge key")
	}
	if !strings.Contains(out, "every forge must still accept it") {
		t.Fatalf("plan does not warn about the replaced forge key:\n%s", out)
	}
}

func TestRestoreNoLogsSkipsTheLogs(t *testing.T) {
	t.Parallel()
	archive, _, _, target := reinstallFixture(t)
	out := runFixtureRestore(t, archive, target, RestoreConfig{NoLogs: true})
	if target.logsRestored != "" || !strings.Contains(out, "not restored (--no-logs)") {
		t.Fatalf("--no-logs restored logs:\n%s", out)
	}
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

// The overlay leaves a tunnel alone when the target's CA cannot be read.
func TestRestoreValuesOverlayWithoutServerCA(t *testing.T) {
	t.Parallel()
	archive := restoreArchive{Values: map[string]any{"watchTunnel": map[string]any{"enabled": true}}}
	all := restoreParts{History: true, Secrets: true, Settings: true}
	compared := restoreTarget{Values: map[string]any{}, OpenBao: openBaoRestorePlan{Known: true}}
	overlay, notes := restoreValuesOverlay(archive, compared, all, DefaultArgoNamespace)
	if _, ok := overlay["watchTunnel"]; ok || len(notes) != 1 || !strings.Contains(notes[0], "could not be read") {
		t.Fatalf("overlay %v notes %v", overlay, notes)
	}
	// A dry run without the token says why the tunnel is not shown.
	overlay, notes = restoreValuesOverlay(archive, restoreTarget{Values: map[string]any{}}, all, DefaultArgoNamespace)
	if _, ok := overlay["watchTunnel"]; ok || len(notes) != 1 || !strings.Contains(notes[0], "without the root token") {
		t.Fatalf("overlay %v notes %v", overlay, notes)
	}
}

// In-flight temp files and anything outside logs/ are dropped, not fatal.
func TestRepackLogsTarKeepsOnlyLogFiles(t *testing.T) {
	t.Parallel()
	body, summary, err := repackLogsTar(logsTarFixture(t, map[string]string{
		"logs/run.log": "x", "logs/run.index.json": "{}", "logs/.index-123.tmp": "partial", "logs/sub/a.log": "y",
	}))
	if err != nil || summary.Files != 2 {
		t.Fatalf("repack: %+v %v", summary, err)
	}
	if _, err := validateLogsTar(body); err != nil {
		t.Fatalf("repacked tar does not validate: %v", err)
	}
}

func TestBackupGoesOnWhenLogsCannotBeRead(t *testing.T) {
	t.Parallel()
	_, _, source := backupFixtureArchive(t, false)
	source.logsTar = []byte("not a tar")
	output := filepath.Join(t.TempDir(), "state.cms")
	var out bytes.Buffer
	cfg := BackupConfig{Output: output, Recipient: "recipient.pem"}
	cfg.applyDefaults()
	if err := runBackup(context.Background(), cfg, source.deps(&out)); err != nil {
		t.Fatalf("backup failed over unreadable logs: %v", err)
	}
	if !strings.Contains(out.String(), "no step logs (could not read them") {
		t.Fatalf("output does not explain the missing logs:\n%s", out.String())
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
	if err := writeBackupArchive(context.Background(), dir, &manifest, fakeEnvelopeCipher{}, output); err != nil {
		t.Fatal(err)
	}
	_, err := openBackupArchive(context.Background(), output, fakeEnvelopeCipher{}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "use the oberth release that wrote it") {
		t.Fatalf("newer format error = %v", err)
	}
}
