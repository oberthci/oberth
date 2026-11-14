package installer

// Every backup/restore scenario an operator meets (#068): full and
// history-only backups, restoring all of an archive or one part, the
// universal secrets export, and what the plan says will change.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/store"
)

// historyBackup writes a --history-only archive of the reinstall source.
func historyBackup(t *testing.T) (string, *fakeBackupCluster, *fakeBackupCluster) {
	t.Helper()
	_, _, source, target := reinstallFixture(t)
	output := filepath.Join(t.TempDir(), "history.tar.gz.enc")
	var out bytes.Buffer
	cfg := BackupConfig{Output: output, Recipient: "recipient.pem", Scope: BackupScopeHistory}
	cfg.applyDefaults()
	deps := source.deps(&out)
	deps.rootToken = "" // a history backup needs no token
	writesBefore := len(source.argvs)
	if err := runBackup(context.Background(), cfg, deps); err != nil {
		t.Fatalf("history backup: %v\n%s", err, out.String())
	}
	for _, argv := range source.argvs[writesBefore:] {
		if slices.Contains(argv, "openbao-0") {
			t.Fatalf("history backup talked to OpenBao: %v", argv)
		}
	}
	if !strings.Contains(out.String(), "history only") {
		t.Fatalf("output does not say history only:\n%s", out.String())
	}
	return output, source, target
}

func TestHistoryOnlyBackupHoldsNoSecrets(t *testing.T) {
	t.Parallel()
	archive, _, _ := historyBackup(t)
	manifest, err := openBackupArchive(context.Background(), archive, fakeEnvelopeCipher{}, t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(manifest.Members))
	for _, member := range manifest.Members {
		names = append(names, member.Name)
	}
	if manifest.Scope != BackupScopeHistory || !slices.Equal(names, []string{backupMemberDatabase}) {
		t.Fatalf("scope %q members %v", manifest.Scope, names)
	}
	if manifest.Contents == nil || manifest.Contents.Runs != 1 {
		t.Fatalf("contents = %+v", manifest.Contents)
	}
}

func TestRestoreHistoryOnlyArchiveNeedsNoToken(t *testing.T) {
	t.Parallel()
	archive, _, target := historyBackup(t)
	before := canonicalJSON(target.bao.kv)
	var out bytes.Buffer
	cfg := RestoreConfig{Input: archive, Yes: true, Timeout: 10 * time.Second}
	cfg.applyDefaults()
	deps := target.deps(&out)
	deps.rootToken = ""
	if err := runRestore(context.Background(), cfg, deps); err != nil {
		t.Fatalf("restore: %v\n%s", err, out.String())
	}
	if canonicalJSON(target.bao.kv) != before || len(target.bao.writes) != 0 {
		t.Fatalf("history restore wrote OpenBao: %v", target.bao.writes)
	}
	for _, argv := range target.argvs {
		if slices.Contains(argv, "openbao-0") {
			t.Fatalf("history restore talked to OpenBao: %v", argv)
		}
	}
	if len(target.swapArgs) != 3 {
		t.Fatalf("database not restored: swap %v", target.swapArgs)
	}
	compat, _ := target.helmValues["compatibility"].(map[string]any)
	if compat["auditDomain"] != sourceCompat.AuditDomain || len(target.helmValues) != 1 {
		t.Fatalf("history restore set %v, want only compatibility", target.helmValues)
	}
	assertSubstringsInOrder(t, out.String(), []string{"holds history only", "What changes on this install:", "History    replaced, not merged",
		"Unchanged  OpenBao secrets, policies, roles and grants"})
}

// During oberth upgrade --rollback the previous release crash-loops on the
// migrated database: its pod is Running but answers no exec. The restore
// treats it as a stopped server and checks the schema in the restore pod.
func TestRestoreOntoACrashLoopingServer(t *testing.T) {
	t.Parallel()
	archive, _, target := historyBackup(t)
	target.schemaExecErr = errors.New("kubectl: exit status 1: error: unable to upgrade connection: container not found (\"oberth\")")
	var out bytes.Buffer
	cfg := RestoreConfig{Input: archive, Yes: true, Only: []string{RestorePartHistory}, Timeout: 10 * time.Second}
	cfg.applyDefaults()
	deps := target.deps(&out)
	deps.rootToken = ""
	if err := runRestore(context.Background(), cfg, deps); err != nil {
		t.Fatalf("restore: %v\n%s", err, out.String())
	}
	if len(target.swapArgs) != 3 {
		t.Fatalf("database not restored: swap %v", target.swapArgs)
	}
	assertSubstringsInOrder(t, out.String(), []string{"no server pod is running: the target schema is checked in the restore pod", "treated as not running"})
}

func TestRestoreOnlyHistoryFromAFullArchive(t *testing.T) {
	t.Parallel()
	archive, _, _, target := reinstallFixture(t)
	out := runFixtureRestore(t, archive, target, RestoreConfig{Only: []string{RestorePartHistory}})
	if len(target.bao.writes) != 0 {
		t.Fatalf("--only history wrote OpenBao: %v", target.bao.writes)
	}
	if len(target.swapArgs) != 3 {
		t.Fatal("--only history did not swap the database")
	}
	// The run that was running in the backup is handed to the server as
	// restored, so startup recovery interrupts it instead of re-running it.
	var marker struct {
		Runs []string `json:"runs"`
	}
	if err := json.Unmarshal(target.restoredRunsMarker, &marker); err != nil || len(marker.Runs) != 1 {
		t.Fatalf("restored-runs marker = %q, %v", target.restoredRunsMarker, err)
	}
	if !strings.Contains(out, "1 running when the backup was taken") {
		t.Fatalf("plan does not name the restored running run:\n%s", out)
	}
	if _, err := target.client.CoreV1().ConfigMaps(DefaultNamespace).Get(context.Background(), secretAccessConfigMapName, metav1GetOptions()); err == nil {
		t.Fatal("--only history wrote the grants table")
	}
	if len(target.helmValues) != 1 {
		t.Fatalf("--only history set more than the compatibility values: %v", target.helmValues)
	}
	if !strings.Contains(out, "Unchanged  OpenBao secrets, policies, roles and grants") {
		t.Fatalf("plan does not say what stays:\n%s", out)
	}
}

// A rollback restore (oberth upgrade --rollback) hands every active run of
// the backup to the server, not only the running ones.
func TestRollbackRestoreHandsQueuedRunsToTheServer(t *testing.T) {
	t.Parallel()
	archive, runID, _, target := reinstallFixture(t)
	// The backup's own Workflow of its running run is kept; one submitted
	// for that run under another name (after the backup) is not.
	target.workflows = append(target.workflows, runWorkflow{Name: "oberth-backup-fixture", RunID: runID, UID: "backup-uid", ResourceVersion: "7", Phase: "Running"})
	out := runFixtureRestore(t, archive, target, RestoreConfig{Only: []string{RestorePartHistory}, InterruptQueued: true})
	if stopped := slices.Sorted(slices.Values(target.stopped)); len(target.deleted) != 0 || !slices.Equal(stopped, []string{"oberth-oberth-known", "oberth-oberth-orphan"}) {
		t.Fatalf("rollback restore deleted=%v stopped=%v, want Stop on the after-backup and unknown Workflows", target.deleted, target.stopped)
	}
	var marker struct {
		Runs []string `json:"runs"`
	}
	if err := json.Unmarshal(target.restoredRunsMarker, &marker); err != nil || len(marker.Runs) != 1 {
		t.Fatalf("restored-runs marker = %q, %v", target.restoredRunsMarker, err)
	}
	if !strings.Contains(out, "queued, waiting or running when the backup was taken") {
		t.Fatalf("plan does not name the rollback's run handling:\n%s", out)
	}
}

func TestRestoreOnlySecrets(t *testing.T) {
	t.Parallel()
	archive, _, source, target := reinstallFixture(t)
	out := runFixtureRestore(t, archive, target, RestoreConfig{Only: []string{RestorePartSecrets}})
	if target.swapArgs != nil {
		t.Fatal("--only secrets touched the database")
	}
	if cm, err := target.client.CoreV1().ConfigMaps(DefaultNamespace).Get(context.Background(), secretAccessConfigMapName, metav1GetOptions()); err != nil || cm.Data[secretAccessConfigMapKey] != testGrants {
		t.Fatalf("--only secrets did not restore the grants: %v %v", cm, err)
	}
	if canonicalJSON(target.bao.kv["release/cosign-secret"]) != canonicalJSON(source.bao.kv["release/cosign-secret"]) {
		t.Fatal("--only secrets did not restore the secrets")
	}
	if target.helmUpgrade != nil {
		t.Fatalf("--only secrets ran a Helm upgrade: %v", target.helmUpgrade)
	}
	if !strings.Contains(out, "Unchanged  history (runs, issues, uplinks)") {
		t.Fatalf("plan does not say what stays:\n%s", out)
	}
}

func TestRestorePartsValidation(t *testing.T) {
	t.Parallel()
	if _, err := resolveRestoreParts([]string{"secrets"}, BackupScopeHistory); err == nil || !strings.Contains(err.Error(), "history only") {
		t.Fatalf("secrets from a history archive: %v", err)
	}
	if _, err := resolveRestoreParts([]string{"history", "settings"}, BackupScopeFull); err == nil || !strings.Contains(err.Error(), `"settings"`) {
		t.Fatalf("unknown part: %v", err)
	}
	parts, err := resolveRestoreParts(nil, BackupScopeHistory)
	if err != nil || parts != (restoreParts{History: true}) {
		t.Fatalf("default parts of a history archive = %+v, %v", parts, err)
	}
	parts, err = resolveRestoreParts(nil, "")
	if err != nil || parts != (restoreParts{History: true, Secrets: true}) {
		t.Fatalf("default parts of a v1 archive = %+v, %v", parts, err)
	}
}

func TestExportSecretsIsUniversalJSON(t *testing.T) {
	t.Parallel()
	archivePath, _, source, _ := reinstallFixture(t)
	work := t.TempDir()
	deps := backupDeps{Deps: Deps{Output: &bytes.Buffer{}}, cipher: fakeEnvelopeCipher{}, now: time.Now}
	archive, err := loadRestoreArchive(context.Background(), RestoreConfig{Input: archivePath}, deps, work)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := writeSecretsExport(&out, archive); err != nil {
		t.Fatal(err)
	}
	var doc SecretsExport
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	operator := 0
	for path := range source.bao.kv {
		if !strings.HasPrefix(path, installIdentityPrefix) {
			operator++
		}
	}
	if doc.Format != secretsExportFormat || doc.KVVersion != 2 || doc.Mount != "oberth" || len(doc.Secrets) != operator {
		t.Fatalf("export header %+v with %d secrets", doc, len(doc.Secrets))
	}
	for _, secret := range doc.Secrets {
		if canonicalJSON(secret.Data) != canonicalJSON(source.bao.kv[secret.Path]) {
			t.Fatalf("secret %s differs from the source", secret.Path)
		}
	}
	history, _, _ := historyBackup(t)
	archive, err = loadRestoreArchive(context.Background(), RestoreConfig{Input: history}, deps, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := writeSecretsExport(&bytes.Buffer{}, archive); err == nil {
		t.Fatal("a history archive exported secrets")
	}
}

func TestInspectShowsContentsWithoutValues(t *testing.T) {
	t.Parallel()
	archivePath, _, _, _ := reinstallFixture(t)
	deps := backupDeps{Deps: Deps{Output: &bytes.Buffer{}}, cipher: fakeEnvelopeCipher{}, now: time.Now}
	var out bytes.Buffer
	deps.Output = &out
	if err := inspectArchive(context.Background(), RestoreConfig{Input: archivePath}, deps); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	assertSubstringsInOrder(t, text, []string{"holds history and secrets", "History    1 run,", "opened under acme-oberth-schema-v1",
		"Secrets    2 KV secrets, 1 policies, 1 roles, 1 grants", "release/cosign-secret", "Nothing was changed (--inspect)"})
	if strings.Contains(text, "identities/") {
		t.Fatalf("inspect lists install identities:\n%s", text)
	}
	for _, value := range []string{"forge-private-canary", testSecretValue, "cosign-canary"} {
		if strings.Contains(text, value) {
			t.Fatalf("inspect printed a secret value %q", value)
		}
	}
}

func TestHistoryChangesNameUplinksAndTokens(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	archive := restoreArchive{Contents: store.ContentSummary{Runs: 2347, Issues: 905, OpenIssues: 6, Uplinks: []string{"admin@example-host (admin)", "operator@example-host"}}}
	target := restoreTarget{Contents: &store.ContentSummary{Runs: 3, Issues: 1, Uplinks: []string{"operator@target-host"}}}
	printHistoryChanges(&out, archive, target)
	assertSubstringsInOrder(t, out.String(), []string{"runs 3 -> 2347, issues 1 -> 905 (6 open)",
		"uplinks removed: operator@target-host — their tokens, including the one this install printed, stop working",
		"uplinks restored: admin@example-host (admin), operator@example-host — their previous tokens work again"})
}

func TestStoreSummaryCountsHistory(t *testing.T) {
	t.Parallel()
	archiveBytes, _, _ := sourceDatabase(t)
	path := filepath.Join(t.TempDir(), "db.tar")
	if err := writeFileForTest(path, archiveBytes); err != nil {
		t.Fatal(err)
	}
	protocol := store.ProtocolConfig{SchemaIdentity: sourceCompat.SchemaIdentity, AuditDomain: sourceCompat.AuditDomain}
	summary, err := summarizeDatabaseArchive(context.Background(), path, t.TempDir(), protocol, 0)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Runs != 1 || !slices.Equal(summary.Repositories, []string{"codeberg/oberth"}) || !slices.Equal(summary.Upstreams, []string{"codeberg"}) {
		t.Fatalf("summary = %+v", summary)
	}
	if got := describeContents(summary); !strings.HasPrefix(got, "1 run, 0 issues (0 open), 0 promotions, 1 repository, 0 uplinks") {
		t.Fatalf("describeContents = %q", got)
	}
}

func metav1GetOptions() metav1.GetOptions { return metav1.GetOptions{} }

func writeFileForTest(path string, body []byte) error { return os.WriteFile(path, body, 0o600) }

// Secrets restore only into the namespaces their roles bind.
func TestRestoreRefusesSecretsIntoOtherNamespaces(t *testing.T) {
	t.Parallel()
	archive, _, _, target := reinstallFixture(t)
	var out bytes.Buffer
	cfg := RestoreConfig{Input: archive, Yes: true, Timeout: 10 * time.Second, ArgoNamespace: "other-argo"}
	cfg.applyDefaults()
	err := runRestore(context.Background(), cfg, target.deps(&out))
	if err == nil || !strings.Contains(err.Error(), "restore into the same namespaces") || len(target.bao.writes) != 0 {
		t.Fatalf("restore into other namespaces = %v, writes %v", err, target.bao.writes)
	}
	cfg.Only = []string{RestorePartHistory}
	out.Reset()
	if err := runRestore(context.Background(), cfg, target.deps(&out)); err != nil {
		t.Fatalf("--only history into other namespaces: %v\n%s", err, out.String())
	}
}
