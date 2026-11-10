package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// upgradeRecordFormat names the rollback record oberth upgrade writes before
// its first change.
const upgradeRecordFormat = "oberth.upgrade-record/v1"

// UpgradeRecord is what a supported rollback needs to know about one
// upgrade: where it ran, the Helm revisions it started from, the database
// schema before and after, and the pre-upgrade backup. oberth upgrade writes
// it before changing anything; oberth upgrade --rollback FILE reads it.
type UpgradeRecord struct {
	Format    string `json:"format"`
	CreatedAt string `json:"createdAt"`
	Context   string `json:"context"`
	APIServer string `json:"apiServer"`
	// ClusterUID is the kube-system namespace UID ("" when unreadable).
	ClusterUID string `json:"clusterUID,omitempty"`
	Namespace  string `json:"namespace"`
	From       string `json:"from"`
	To         string `json:"to"`
	// OberthRevision is the deployed oberth Helm revision before the upgrade.
	OberthRevision int `json:"oberthRevision"`
	// ArgoNamespace and ArgoRevision are set when the upgrade also changes
	// the argo-workflows release; ArgoRevision is its revision before.
	ArgoNamespace string `json:"argoNamespace,omitempty"`
	ArgoRevision  int    `json:"argoRevision,omitempty"`
	// SchemaBefore is the running database schema (0 when unknown);
	// SchemaAfter the schema the target server migrates to.
	SchemaBefore int `json:"schemaBefore"`
	SchemaAfter  int `json:"schemaAfter"`
	// Backup is the pre-upgrade backup archive ("" with --no-backup).
	Backup string `json:"backup,omitempty"`
}

// migrates reports whether the upgrade moves the database schema forward,
// or may (unknown before): rolling it back then needs the backup.
func (r UpgradeRecord) migrates() bool {
	return r.SchemaBefore <= 0 || r.SchemaAfter > r.SchemaBefore
}

func helmRevisionNumber(raw json.RawMessage) int {
	label := strings.TrimPrefix(helmRevisionLabel(raw), " revision ")
	n, err := strconv.Atoi(label)
	if err != nil {
		return 0
	}
	return n
}

func kubeSystemUID(ctx context.Context, deps Deps) string {
	if deps.KubeClient == nil {
		return ""
	}
	ns, err := deps.KubeClient.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return string(ns.UID)
}

// takeUpgradeBackup runs the pre-upgrade backup, or records the explicit
// --no-backup. Without either the upgrade refuses: it is the one command
// that knows a migration may be coming.
func takeUpgradeBackup(ctx context.Context, cfg UpgradeConfig, w io.Writer) (string, error) {
	if cfg.NoBackup {
		_, _ = fmt.Fprintln(w, "WARNING: --no-backup: no pre-upgrade backup is taken; a rollback across a schema migration will not be possible.")
		return "", nil
	}
	if cfg.Backup == nil {
		return "", errors.New("no pre-upgrade backup is configured; rerun through the oberth CLI, or pass --no-backup to upgrade without one")
	}
	_, _ = fmt.Fprintln(w, "\nTaking the pre-upgrade backup (history only)...")
	backup, err := cfg.Backup(ctx)
	if err != nil {
		return "", fmt.Errorf("pre-upgrade backup failed, nothing was changed: %w (pass --no-backup to upgrade without one)", err)
	}
	if strings.TrimSpace(backup.Path) == "" {
		return "", errors.New("pre-upgrade backup reported no archive; nothing was changed")
	}
	return backup.Path, nil
}

// writeUpgradeRecord writes the rollback record before the first change.
func writeUpgradeRecord(cfg UpgradeConfig, record UpgradeRecord) (string, error) {
	dir := cfg.RecordDir
	if dir == "" {
		if record.Backup != "" {
			dir = filepath.Dir(record.Backup)
		} else {
			dir = "."
		}
	}
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return "", err
	}
	stem := fmt.Sprintf("oberth-upgrade-%s-to-%s-%s", displayVersion(record.From), displayVersion(record.To),
		strings.NewReplacer(":", "", "-", "").Replace(record.CreatedAt))
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	// The record is created inside the chosen directory only: the name is
	// fixed, and os.Root refuses anything that would leave the directory.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", fmt.Errorf("write rollback record: %w", err)
	}
	defer func() { _ = root.Close() }()
	var file *os.File
	var path string
	for attempt := 1; ; attempt++ {
		name := stem + ".json"
		if attempt > 1 {
			name = fmt.Sprintf("%s-%d.json", stem, attempt)
		}
		path = filepath.Join(dir, name)
		file, err = root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) || attempt == 100 {
			return "", fmt.Errorf("write rollback record: %w", err)
		}
	}
	_, writeErr := file.Write(append(raw, '\n'))
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return "", fmt.Errorf("write rollback record: %w", err)
	}
	return path, nil
}

// RollbackConfig selects an upgrade record to roll back.
type RollbackConfig struct {
	Record    string
	Namespace string
	DryRun    bool
	Yes       bool
	// ForceRecord accepts a record whose Helm revisions are not the ones the
	// recorded upgrade left (a later change to either release).
	ForceRecord bool
	Timeout     time.Duration
	// Restore restores the record's backup (history only) onto the rolled
	// back release; the CLI wires it to oberth restore.
	Restore func(ctx context.Context, restore RollbackRestore) error
}

// RollbackRestore names what a rollback restores and where: the record's
// installation, never the CLI's default namespaces.
type RollbackRestore struct {
	Namespace     string // the record's Oberth namespace
	ArgoNamespace string // that release's pipeline namespace
	Archive       string // the pre-upgrade backup
}

// releasePipelineNamespace reads argo.namespace of the oberth release in
// namespace ("" when its values name none).
func releasePipelineNamespace(ctx context.Context, deps Deps, namespace string) (string, error) {
	raw, err := deps.RunHelm(ctx, []string{"get", "values", "oberth", "-n", namespace, "--all", "-o", "json"})
	if err != nil {
		return "", fmt.Errorf("read the pipeline namespace of release oberth in %s: %w", namespace, err)
	}
	var values struct {
		Argo struct {
			Namespace string `json:"namespace"`
		} `json:"argo"`
	}
	if err := json.Unmarshal(raw, &values); err != nil {
		return "", fmt.Errorf("read the pipeline namespace of release oberth in %s: %w", namespace, err)
	}
	return values.Argo.Namespace, nil
}

// ReadUpgradeRecord reads and checks a rollback record.
func ReadUpgradeRecord(path string) (UpgradeRecord, error) {
	var record UpgradeRecord
	raw, err := readFileWithin(path, 64<<10)
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		return record, fmt.Errorf("decode rollback record: %w", err)
	}
	if record.Format != upgradeRecordFormat || record.Namespace == "" || record.OberthRevision <= 0 ||
		canonicalChartVersion(record.From) == "" || canonicalChartVersion(record.To) == "" {
		return record, fmt.Errorf("%s is not an oberth upgrade record", path)
	}
	return record, nil
}

// RunRollback rolls one recorded upgrade back: the argo-workflows release (if
// the upgrade changed it) and the oberth release to their recorded
// revisions, then, when the upgrade migrated the database, the pre-upgrade
// backup's history through oberth restore, because the previous server cannot
// open the migrated database.
func RunRollback(ctx context.Context, cfg RollbackConfig, deps Deps) error {
	w := deps.Output
	if w == nil {
		w = io.Discard
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	record, err := ReadUpgradeRecord(cfg.Record)
	if err != nil {
		return err
	}
	if cfg.Namespace != "" && cfg.Namespace != record.Namespace {
		return fmt.Errorf("the record is for namespace %s, not %s", record.Namespace, cfg.Namespace)
	}
	server := ""
	if deps.RestConfig != nil {
		server = deps.RestConfig.Host
	}
	if record.APIServer != "" && server != record.APIServer {
		return fmt.Errorf("the record is for API server %s, but context %q is %s; select the cluster the upgrade ran on", record.APIServer, deps.ContextName, server)
	}
	if uid := kubeSystemUID(ctx, deps); record.ClusterUID != "" && uid != "" && uid != record.ClusterUID {
		return errors.New("the record is for another cluster (kube-system UID differs); refusing to roll back here")
	}
	if !IsLocalServer(server) && !cfg.Yes {
		return fmt.Errorf("target %q does not appear to be a local cluster (server: %s); use --yes to proceed", deps.ContextName, server)
	}
	release, exists, err := findHelmRelease(ctx, deps, "oberth", record.Namespace)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("no Oberth Helm release in namespace %s", record.Namespace)
	}
	current := chartVersionFromRelease(release.Chart, "oberth")
	if canonicalChartVersion(current) != canonicalChartVersion(record.To) && canonicalChartVersion(current) != canonicalChartVersion(record.From) {
		return fmt.Errorf("the release runs %s, neither the upgrade's target %s nor its source %s; it moved since this record, refusing to roll back",
			displayVersion(current), displayVersion(record.To), displayVersion(record.From))
	}
	// Bind the record to the revisions its upgrade left: one helm upgrade
	// after the recorded one, or two once a rollback of it already ran. Any
	// later change to the release makes the record stale.
	if err := checkRecordRevision("oberth", record.Namespace, record.OberthRevision, release, cfg.ForceRecord); err != nil {
		return err
	}
	if record.ArgoRevision > 0 {
		argo, found, err := findHelmRelease(ctx, deps, "argo-workflows", record.ArgoNamespace)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("no argo-workflows Helm release in namespace %s", record.ArgoNamespace)
		}
		if err := checkRecordRevision("argo-workflows", record.ArgoNamespace, record.ArgoRevision, argo, cfg.ForceRecord); err != nil {
			return err
		}
	}
	needRestore := record.migrates()
	if needRestore && record.Backup == "" {
		return fmt.Errorf("the upgrade from %s to %s migrated (or may have migrated) the database and took no backup (--no-backup): "+
			"the previous server cannot open the migrated database; fix forward with oberth upgrade instead", displayVersion(record.From), displayVersion(record.To))
	}
	restore := RollbackRestore{Namespace: record.Namespace, Archive: record.Backup}
	if needRestore {
		if _, err := os.Stat(record.Backup); err != nil {
			return fmt.Errorf("pre-upgrade backup %s: %w", record.Backup, err)
		}
		if restore.ArgoNamespace, err = releasePipelineNamespace(ctx, deps, record.Namespace); err != nil {
			return err
		}
	}

	_, _ = fmt.Fprintf(w, "Rollback of the upgrade %s -> %s in namespace %s (context %s):\n", displayVersion(record.From), displayVersion(record.To), record.Namespace, deps.ContextName)
	step := 1
	if record.ArgoRevision > 0 {
		_, _ = fmt.Fprintf(w, "  %d. helm rollback argo-workflows %d -n %s --wait\n", step, record.ArgoRevision, record.ArgoNamespace)
		step++
	}
	oberthArgs := []string{"rollback", "oberth", strconv.Itoa(record.OberthRevision), "-n", record.Namespace, "--timeout", cfg.Timeout.String()}
	if !needRestore {
		oberthArgs = append(oberthArgs, "--wait")
	}
	_, _ = fmt.Fprintf(w, "  %d. helm %s\n", step, strings.Join(oberthArgs, " "))
	step++
	if needRestore {
		_, _ = fmt.Fprintf(w, "  %d. restore the history of %s (schema %d; the database was migrated to %d): oberth restore %s --only history --yes\n",
			step, record.Backup, record.SchemaBefore, record.SchemaAfter, record.Backup)
		step++
	}
	_, _ = fmt.Fprintf(w, "  %d. verify the server runs %s\n", step, displayVersion(record.From))
	if needRestore {
		_, _ = fmt.Fprintf(w, "The restore replaces the database with the backup of %s: history recorded since then is not kept.\n", displayOr(record.CreatedAt, "the upgrade"))
	}
	if cfg.DryRun {
		_, _ = fmt.Fprintln(w, "No changes were made (--dry-run).")
		return nil
	}
	// A restore replaces the database, on a local cluster too: like oberth
	// restore, it never runs on the record's name alone.
	if needRestore && !cfg.Yes {
		return errors.New("this rollback replaces the server database with the pre-upgrade backup; review the plan above and rerun with --yes")
	}
	if needRestore && cfg.Restore == nil {
		return errors.New("the rollback needs a restore of the backup, and no restore is configured")
	}

	if record.ArgoRevision > 0 {
		if _, err := deps.RunHelm(ctx, []string{"rollback", "argo-workflows", strconv.Itoa(record.ArgoRevision), "-n", record.ArgoNamespace, "--wait", "--timeout", cfg.Timeout.String()}); err != nil {
			return fmt.Errorf("helm rollback argo-workflows: %w", err)
		}
		_, _ = fmt.Fprintf(w, "argo-workflows rolled back to revision %d\n", record.ArgoRevision)
	}
	if _, err := deps.RunHelm(ctx, oberthArgs); err != nil {
		return fmt.Errorf("helm rollback oberth: %w", err)
	}
	_, _ = fmt.Fprintf(w, "oberth rolled back to revision %d\n", record.OberthRevision)
	if needRestore {
		if err := cfg.Restore(ctx, restore); err != nil {
			return fmt.Errorf("restore the pre-upgrade backup: %w", err)
		}
	}
	running, err := verifyRunningVersion(ctx, deps, UpgradeConfig{Namespace: record.Namespace}, record.From)
	if err != nil {
		return fmt.Errorf("rollback applied but the running version is unverified: %w", err)
	}
	// upgrade.completed is the lifecycle action every server version accepts.
	recordServerAudit(ctx, deps, record.Namespace, "deploy/oberth", "upgrade.completed", map[string]any{"from": record.To, "to": record.From, "rollback": true})
	_, _ = fmt.Fprintf(w, "\nRolled back: Oberth %s in namespace %s (context %s)\n", running, record.Namespace, deps.ContextName)
	return nil
}

// checkRecordRevision refuses a record when the release's deployed revision
// is not the one the recorded upgrade made (recorded+1), or the one a
// rollback of it made (recorded+2), unless force.
func checkRecordRevision(name, namespace string, recorded int, release helmRelease, force bool) error {
	deployed := helmRevisionNumber(release.Revision)
	if deployed == recorded+1 || deployed == recorded+2 || force {
		return nil
	}
	return fmt.Errorf("release %s in %s is at revision %d, but this record's upgrade started from revision %d: "+
		"the release changed after that upgrade, so the record is stale; refusing to roll back (--force-record overrides after checking helm history %s -n %s)",
		name, namespace, deployed, recorded, name, namespace)
}

// readFileWithin reads one regular file of at most limit bytes, opened
// through an os.Root on its own directory so the name cannot leave it.
func readFileWithin(path string, limit int64) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(filepath.Base(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, limit)
	}
	return raw, nil
}
