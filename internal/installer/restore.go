package installer

// `oberth restore`: rebuild an installation's state from an `oberth backup`
// archive onto a (typically fresh) install. It automates the hand restore of
// an installation on 2026-10-16 (issues 038 and 048): validate the database offline,
// write OpenBao state field-for-field and verify it by read-back, scale the
// server to zero, swap the database on its volume keeping the previous one
// aside, restore the secret-access table, stop Argo Workflows whose runs
// only the replaced database knew, set the compatibility values the restored
// database needs, and scale back up.
//
// Nothing changes before the archive, the database and the target have all
// been checked and the operator has confirmed the printed plan.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/mod/semver"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/oberthci/oberth/internal/auditanchor"
	"github.com/oberthci/oberth/internal/containerimage"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// RestoreConfig selects the archive, its key, and the target installation.
type RestoreConfig struct {
	MaxDatabaseBytes int64 // same logical snapshot limit as backup

	KubeconfigPath   string
	ContextName      string
	Namespace        string
	OpenBaoNamespace string
	ArgoNamespace    string
	// UtilityImage overrides the installed file-tool image for database restore.
	UtilityImage string
	Input        string
	PrivateKey   string
	// ChartPath is the local chart used when compatibility values must be
	// set on the release (development; not verified); empty selects the
	// signed release at the version the release already runs, verified
	// before Helm sees it.
	ChartPath string
	// Release is where that signed release is read from.
	Release ReleaseSource
	// Only limits the restore to these parts (RestorePartHistory,
	// RestorePartSecrets); empty restores everything the archive holds.
	Only []string
	// Inspect prints what the archive holds and stops; no cluster is used.
	Inspect bool
	// InterruptQueued (set by oberth upgrade --rollback) also hands the
	// backup's queued and waiting runs to the server as restored: they may
	// already have run on the version the rollback undid.
	InterruptQueued bool
	DryRun          bool
	Yes             bool
	Timeout         time.Duration
}

func (cfg *RestoreConfig) applyDefaults() {
	if cfg.Namespace == "" {
		cfg.Namespace = DefaultNamespace
	}
	if cfg.OpenBaoNamespace == "" {
		cfg.OpenBaoNamespace = DefaultOpenBaoNamespace
	}
	if cfg.ArgoNamespace == "" {
		cfg.ArgoNamespace = DefaultArgoNamespace
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
}

// ExecuteRestore resolves the cluster and host tools and runs the restore.
func ExecuteRestore(ctx context.Context, cfg RestoreConfig, host InstallDeps) error {
	cfg.applyDefaults()
	if _, err := store.BackupMaxBytes(cfg.MaxDatabaseBytes); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Input) == "" {
		return errors.New("--input is required: the archive written by oberth backup")
	}
	if strings.TrimSpace(cfg.PrivateKey) == "" {
		return errors.New("--private-key is required: the PEM private key of the archive's recipient")
	}
	if _, err := os.Stat(cfg.Input); err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	if err := validateBackupPrivateKey(cfg.PrivateKey); err != nil {
		return err
	}
	if cfg.Inspect {
		host = host.withDefaults()
		openssl, err := host.LookPath("openssl")
		if err != nil {
			return errors.New("openssl not found in PATH; backup archives use OpenSSL 3 `cms` (AES-256-GCM), install it and rerun")
		}
		deps := backupDeps{Deps: Deps{Output: host.Output}, openssl: openssl, now: time.Now}
		deps.cipher = opensslCMS{binary: openssl, privateKey: cfg.PrivateKey}
		return inspectArchive(ctx, cfg, deps)
	}
	deps, err := prepareOperatorDeps(host, cfg.KubeconfigPath, cfg.ContextName, !cfg.DryRun)
	if err != nil {
		return err
	}
	deps.cipher = opensslCMS{binary: deps.openssl, privateKey: cfg.PrivateKey}
	return runRestore(ctx, cfg, deps)
}

// inspectArchive decrypts and verifies an archive and prints what it holds.
func inspectArchive(ctx context.Context, cfg RestoreConfig, deps backupDeps) (resultErr error) {
	work, err := os.MkdirTemp("", "oberth-restore-")
	if err != nil {
		return fmt.Errorf("create private working directory: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(work)) }()
	_, _ = fmt.Fprintf(deps.Output, "Decrypting and verifying %s...\n", displayLocalPath(cfg.Input))
	archive, err := loadRestoreArchive(ctx, cfg, deps, work)
	if err != nil {
		return err
	}
	printArchiveContents(deps.Output, archive, cfg.Input, true)
	_, _ = fmt.Fprintf(deps.Output, "\nNothing was changed (--inspect). To see what a restore would change on this cluster: oberth restore %s --dry-run\n", displayLocalPath(cfg.Input))
	return nil
}

// restoreArchive is a decrypted, verified archive ready to apply.
type restoreArchive struct {
	Manifest  backupManifest
	CipherSHA string
	KV        kvExport
	Policies  []policyExport
	Roles     rolesExport
	Grants    secretAccessExport
	// Reference is the archive's reference-only record (#074 reads the
	// server CA it names); zero when the archive has none.
	Reference openBaoReference
	Report    store.BackupReport
	Database  restoredDatabase
	// LocalDatabase is the verified pair written for local inspection.
	LocalDatabase string
	// SkippedIdentities counts install-owned identities an older archive
	// carried, which a restore leaves out.
	SkippedIdentities int
	// Contents counts what the archived database holds.
	Contents store.ContentSummary
	// RestoredHeadID and RestoredHeadSHA256 are the audit head of the
	// database the server starts on, after any recorded restoration action.
	RestoredHeadID     int64
	RestoredHeadSHA256 string
}

func (a restoreArchive) restoredHeadID() int64 {
	if a.RestoredHeadSHA256 != "" {
		return a.RestoredHeadID
	}
	return a.Manifest.Database.AuditHeadID
}

func (a restoreArchive) restoredHeadSHA256() string {
	return displayOr(a.RestoredHeadSHA256, a.Manifest.Database.AuditHeadSHA256)
}

// loadRestoreArchive decrypts the archive, checks every member hash, and
// validates the database offline under the recorded protocol identifiers.
func loadRestoreArchive(ctx context.Context, cfg RestoreConfig, deps backupDeps, work string) (restoreArchive, error) {
	var archive restoreArchive
	sha, err := fileSHA256(cfg.Input)
	if err != nil {
		return archive, fmt.Errorf("read archive: %w", err)
	}
	archive.CipherSHA = sha
	if archive.Manifest, err = openBackupArchive(ctx, cfg.Input, deps.cipher, work, cfg.MaxDatabaseBytes); err != nil {
		return archive, err
	}
	if archive.Manifest.Scope == BackupScopeHistory {
		archive.KV, archive.Roles = kvExport{Mount: defaultKVPrefix}, rolesExport{Mount: defaultAuthMount}
	} else {
		for _, member := range []struct {
			name  string
			value any
		}{
			{backupMemberKV, &archive.KV},
			{backupMemberPolicies, &archive.Policies},
			{backupMemberRoles, &archive.Roles},
		} {
			if err := readBackupJSON(work, member.name, member.value); err != nil {
				return archive, err
			}
		}
		// Reference-only: read leniently, since older releases recorded
		// other fields, and a restore never applies it.
		if body, err := os.ReadFile(filepath.Join(work, filepath.FromSlash(backupMemberReference))); err == nil { // #nosec G304 -- fixed member name inside the private working directory.
			_ = json.Unmarshal(body, &archive.Reference)
		}
	}
	// Archives from v0.17.33 and earlier also carried the install's own
	// identities; a restore never writes them.
	kept := operatorSecrets(archive.KV.Secrets)
	archive.SkippedIdentities = len(archive.KV.Secrets) - len(kept)
	archive.KV.Secrets = kept
	archive.Policies, archive.Roles = operatorPolicies(archive.Policies), operatorRoles(archive.Roles)
	if archive.KV.Mount != defaultKVPrefix || archive.Roles.Mount != defaultAuthMount {
		return archive, fmt.Errorf("archive covers KV mount %q and auth mount %q; this release restores only %q and %q",
			archive.KV.Mount, archive.Roles.Mount, defaultKVPrefix, defaultAuthMount)
	}
	for _, policy := range archive.Policies {
		if builtinPolicies[policy.Name] {
			return archive, fmt.Errorf("archive carries built-in policy %q; refusing to overwrite it", policy.Name)
		}
	}

	compat := archive.Manifest.Compatibility
	protocol := store.ProtocolConfig{SchemaIdentity: compat.SchemaIdentity, AuditDomain: compat.AuditDomain}
	if err := protocol.Validate(); err != nil {
		return archive, fmt.Errorf("archive compatibility values: %w", err)
	}
	if err := auditanchor.ValidateWitnessKeyInfo(compat.WitnessKeyInfo); err != nil {
		return archive, fmt.Errorf("archive compatibility values: %w", err)
	}
	databaseMember := filepath.Join(work, filepath.FromSlash(backupMemberDatabase))
	if archive.Report, err = verifyDatabaseArchive(ctx, databaseMember, archive.Manifest.Database.Schema, protocol, cfg.MaxDatabaseBytes); err != nil {
		return archive, err
	}
	if archive.Report.AuditHead.ID != archive.Manifest.Database.AuditHeadID ||
		hex.EncodeToString(archive.Report.AuditHead.SHA256) != archive.Manifest.Database.AuditHeadSHA256 {
		return archive, errors.New("the verified database audit head differs from the one the manifest records")
	}
	if archive.Database, err = extractDatabaseFiles(ctx, databaseMember, work, cfg.MaxDatabaseBytes); err != nil {
		return archive, err
	}
	archive.LocalDatabase = archive.Database.Main
	if archive.Contents, err = summarizeDatabase(ctx, archive.LocalDatabase, protocol); err != nil {
		return archive, err
	}
	inspection, err := store.InspectCurrent(ctx, archive.LocalDatabase, store.Options{Protocol: protocol, SnapshotTempDir: filepath.Dir(archive.LocalDatabase)})
	if err != nil {
		return archive, err
	}
	archive.Grants, err = exportDatabaseGrants(ctx, inspection)
	err = errors.Join(err, inspection.Close())
	if err != nil {
		return archive, err
	}
	if err := validateExportedGrants(archive.Grants.Grants); err != nil {
		return archive, err
	}
	_, _ = fmt.Fprintf(deps.Output, "Verified: all %d parts match the manifest; the database (schema %d) passes integrity, foreign-key and audit-chain checks offline.\n",
		len(archive.Manifest.Members), archive.Report.Schema)
	return archive, nil
}

// restoreTarget is what the plan learned about the target installation.
// previousFiles is where the swap moves the target's previous database.
func (t restoreTarget) previousFiles() string {
	return fmt.Sprintf("/data/pre-restore-%s/ on PVC %s", t.Timestamp, t.Server.ClaimName)
}

type restoreTarget struct {
	DeployPublicKey string
	Server          serverDeployment
	Pod             string
	ChartVersion    string
	// Schema is the target server's schema version, 0 when no server pod
	// is running to ask (it is then checked in the helper pod).
	Schema    int
	OpenBao   openBaoRestorePlan
	Overlay   map[string]any
	Notes     []string
	Orphans   []runWorkflow
	Timestamp string
	// ClusterUID identifies the target cluster ("" when unknown).
	ClusterUID string
	// Contents is the target's own history, nil when it could not be read
	// (ContentsNote says why).
	Contents     *store.ContentSummary
	ContentsNote string
	// Grants counts the target's current grants.
	Grants int
	// RestoredRunning are the runs the backup recorded as running; the
	// server interrupts them instead of re-executing them.
	RestoredRunning []string
	// KV is the archive's secrets as they will be written: known_hosts pins
	// of this server updated (#074).
	KV   kvExport
	Pins serverPins
}

func runRestore(ctx context.Context, cfg RestoreConfig, deps backupDeps) (resultErr error) {
	deps.maxDatabaseBytes = cfg.MaxDatabaseBytes
	w := deps.Output
	work, err := os.MkdirTemp("", "oberth-restore-")
	if err != nil {
		return fmt.Errorf("create private working directory: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(work)) }()

	_, _ = fmt.Fprintf(w, "Decrypting and verifying %s...\n", displayLocalPath(cfg.Input))
	archive, err := loadRestoreArchive(ctx, cfg, deps, work)
	if err != nil {
		return err
	}
	parts, err := resolveRestoreParts(cfg.Only, archive.Manifest.Scope)
	if err != nil {
		return err
	}
	printArchiveContents(w, archive, cfg.Input, false)
	// Only restoring secrets needs OpenBao's root token.
	if parts.Secrets && deps.rootToken == "" {
		if deps.rootToken, err = deps.takeRootToken("OpenBao root token of this install (echo disabled): "); err != nil {
			return err
		}
		if deps.rootToken == "" && !cfg.DryRun {
			return fmt.Errorf("%w\n(or restore without secrets: oberth restore %s --only history)", missingRootTokenError("restore"), displayLocalPath(cfg.Input))
		}
	}

	target, bao, err := inspectRestoreTarget(ctx, cfg, deps, archive, parts, work)
	if err != nil {
		return err
	}
	printRestorePlan(w, cfg, deps, archive, target, parts)
	if cfg.DryRun {
		if _, err := previewRestoreHelm(ctx, cfg, deps, target, work); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(w, "\nNo changes were made (--dry-run).")
		return nil
	}
	if err := confirmRestore(cfg, deps); err != nil {
		return err
	}
	return applyRestore(ctx, cfg, deps, archive, target, bao, parts, work)
}

// inspectRestoreTarget reads (never writes) the target installation.
func inspectRestoreTarget(ctx context.Context, cfg RestoreConfig, deps backupDeps, archive restoreArchive, parts restoreParts, work string) (restoreTarget, openBaoExec, error) {
	target := restoreTarget{Timestamp: deps.now().UTC().Format("20060102T150405Z")}
	var err error
	if target.Server, err = inspectServerDeployment(ctx, deps.Deps, cfg.Namespace); err != nil {
		return target, openBaoExec{}, err
	}
	release, found, err := findHelmRelease(ctx, deps.Deps, "oberth", cfg.Namespace)
	if err != nil {
		return target, openBaoExec{}, err
	}
	if !found {
		return target, openBaoExec{}, fmt.Errorf("no oberth Helm release in namespace %s; install Oberth on the target first (oberth setup)", cfg.Namespace)
	}
	target.ChartVersion = chartVersionFromRelease(release.Chart, "oberth")
	if target.Pod, err = runningServerPod(ctx, deps.Deps, cfg.Namespace, target.Server.Selector); err != nil {
		return target, openBaoExec{}, err
	}
	backupSchema := archive.Manifest.Database.Schema
	if target.Pod != "" {
		// A pod whose server cannot answer (crash-looping, as the previous
		// release does on a migrated database during oberth upgrade
		// --rollback) is a server that is not running: the restore pod then
		// checks the target schema before the swap, as for a stopped server.
		if target.Schema, err = podSchemaVersion(ctx, deps.Deps, cfg.Namespace); err != nil {
			target.Notes = append(target.Notes, fmt.Sprintf("server pod %s does not answer (%v); treated as not running", target.Pod, err))
			target.Pod, target.Schema = "", 0
		}
	}
	if target.Pod != "" {
		if parts.History && target.Schema < backupSchema {
			return target, openBaoExec{}, fmt.Errorf("the target server writes schema %d but the backup holds schema %d; upgrade the target to a release at least as new as %s first",
				target.Schema, backupSchema, displayOr(archive.Manifest.Source.ServerVersion, "the source"))
		}
	}
	target.ClusterUID = clusterUID(ctx, deps)

	if parts.History {
		if cfg.InterruptQueued && (target.ChartVersion == "" || semver.Compare(canonicalChartVersion(target.ChartVersion), "v0.21.0-0") < 0) {
			target.Notes = append(target.Notes, "The target predates restored-run protection: queued or running backup work may execute again. Scale the target server to zero and inspect restored work before resuming it.")
		}
		if target.RestoredRunning, err = restoredRunningRuns(ctx, archive, cfg.InterruptQueued); err != nil {
			return target, openBaoExec{}, err
		}
		readTargetContents(ctx, cfg, deps, &target, work)
		target.Overlay = compatibilityOverlay(archive, target)
		workflows, listErr := listRunWorkflows(ctx, deps, cfg.ArgoNamespace)
		if listErr != nil {
			target.Notes = append(target.Notes, fmt.Sprintf("could not list Argo Workflows in %s (%v); orphaned workflows are not cleaned up", cfg.ArgoNamespace, listErr))
		} else if target.Orphans, err = orphanedWorkflows(ctx, workflows, archive, cfg.InterruptQueued); err != nil {
			return target, openBaoExec{}, err
		}
	}
	var bao openBaoExec
	if parts.Secrets {
		// Per-repository roles bind the source's namespaces: restoring them
		// elsewhere would grant nothing, or the wrong service accounts.
		src := archive.Manifest.Source
		if src.Namespace != "" && (src.Namespace != cfg.Namespace || src.ArgoNamespace != cfg.ArgoNamespace) {
			return target, openBaoExec{}, fmt.Errorf("the backup's roles bind namespaces %s/%s, but this install uses %s/%s: "+
				"restore into the same namespaces, or restore only the history with --only history",
				src.Namespace, src.ArgoNamespace, cfg.Namespace, cfg.ArgoNamespace)
		}
		if grants, err := readSecretAccess(ctx, deps.Deps, cfg.Namespace); err == nil {
			target.Grants = countGrants(grants)
		}
		if bao, err = connectOpenBao(ctx, deps.Deps, cfg.OpenBaoNamespace); err != nil {
			return target, openBaoExec{}, err
		}
		target.KV = archive.KV
		target.Pins.Note = "not checked without the root token"
		if deps.rootToken != "" {
			target.KV, target.Pins = checkServerPins(ctx, deps.Deps, cfg.Namespace, target.Server, bao, deps.rootToken, archive)
			if target.OpenBao, err = planOpenBaoRestore(ctx, bao, deps.rootToken, target.KV, archive.Policies, archive.Roles); err != nil {
				return target, openBaoExec{}, err
			}
		}
	}
	return target, bao, nil
}

// readTargetContents counts the history the restore replaces, from a
// verified snapshot the target's running server takes of its own database.
func readTargetContents(ctx context.Context, cfg RestoreConfig, deps backupDeps, target *restoreTarget, work string) {
	if target.Pod == "" {
		target.ContentsNote = "the server is not running"
		return
	}
	protocol := store.ProtocolConfig{SchemaIdentity: target.Server.Compatibility.SchemaIdentity, AuditDomain: target.Server.Compatibility.AuditDomain}
	dir, err := os.MkdirTemp(work, "target-db-")
	if err != nil {
		target.ContentsNote = err.Error()
		return
	}
	snapshot := filepath.Join(dir, "oberth-database.tar")
	if _, err := snapshotServerDatabase(ctx, deps, cfg.Namespace, snapshot); err != nil {
		target.ContentsNote = err.Error()
		return
	}
	summary, err := summarizeDatabaseArchive(ctx, snapshot, dir, protocol, cfg.MaxDatabaseBytes)
	if err != nil {
		target.ContentsNote = err.Error()
		return
	}
	target.Contents = &summary
}

func displayOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// compatibilityOverlay returns the only Helm values a restore sets: the
// compatibility values the restored database can only be opened under.
// Everything else on the release is the target install's or the operator's.
func compatibilityOverlay(archive restoreArchive, target restoreTarget) map[string]any {
	want := archive.Manifest.Compatibility
	if target.Server.Compatibility == want {
		return nil
	}
	return map[string]any{"compatibility": map[string]any{
		"schemaIdentity": want.SchemaIdentity,
		"auditDomain":    want.AuditDomain,
		"witnessKeyInfo": want.WitnessKeyInfo,
	}}
}

func countGrants(grants secretAccessExport) int { return len(grants.Grants) }

// confirmRestore requires --yes, or the target context typed back on a
// terminal: a restore replaces live secrets and the live database.
func confirmRestore(cfg RestoreConfig, deps backupDeps) error {
	if cfg.Yes {
		return nil
	}
	if deps.IsTerminal == nil || !deps.IsTerminal() || deps.Input == nil {
		return errors.New("restore replaces live OpenBao state and the server database; review the plan above and rerun with --yes")
	}
	// The prompt quotes the name: "(default)" read as a default to accept
	// with Enter, and one mistyped answer cancelled the restore (#065).
	reader := bufio.NewReader(deps.Input)
	for attempt := 1; ; attempt++ {
		_, _ = fmt.Fprintf(deps.Output, "\nTo restore onto context %q, type %s and press Enter (Ctrl-C cancels): ", deps.ContextName, deps.ContextName)
		line, err := reader.ReadString('\n')
		answer := strings.TrimSpace(line)
		if answer == deps.ContextName {
			return nil
		}
		if err != nil {
			return errors.New("restore cancelled")
		}
		if attempt == 3 {
			return fmt.Errorf("restore cancelled: typed %q three times, expected %q", answer, deps.ContextName)
		}
		_, _ = fmt.Fprintf(deps.Output, "  %q is not %q; nothing was changed.", answer, deps.ContextName)
	}
}

// applyRestore performs the confirmed plan. Once the server has been scaled
// to zero, every failure says how to get back to a running server.
func applyRestore(ctx context.Context, cfg RestoreConfig, deps backupDeps, archive restoreArchive, target restoreTarget, bao openBaoExec, parts restoreParts, work string) error {
	w := deps.Output
	_, _ = fmt.Fprintln(w)
	// Resolve the chart before changing anything, so a missing chart cannot
	// leave secrets replaced or the server scaled down.
	helmArgs, err := previewRestoreHelm(ctx, cfg, deps, target, work)
	if err != nil {
		return err
	}
	if parts.History {
		// A stopped server's schema is known only once the restore pod
		// starts; swapDatabase rehearses the migration then.
		if target.Schema > 0 {
			if err := preflightRestoreMigration(ctx, archive, target.Schema, target.previousFiles(), w); err != nil {
				return err
			}
		}
	}

	if parts.Secrets {
		_, _ = fmt.Fprintln(w, "Restoring OpenBao...")
		written, err := restoreOpenBao(ctx, bao, deps.rootToken, target.KV, archive.Policies, archive.Roles)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(w, "  ✓ %d secrets (%d written, %d already current), %d policies, %d roles verified by read-back\n",
			len(target.KV.Secrets), written, len(target.KV.Secrets)-written, len(archive.Policies), len(archive.Roles.Roles))
		// Only what this run wrote is "updated"; a secret already holding
		// the rewritten value was left alone (#074 review).
		if updated, current := splitPinRewrites(target.Pins.Rewritten, target.OpenBao); len(updated) > 0 || len(current) > 0 {
			if len(updated) > 0 {
				_, _ = fmt.Fprintf(w, "  ✓ known_hosts pins of this server updated to its SSH host key in %s\n", describePinRewrites(updated))
			}
			if len(current) > 0 {
				_, _ = fmt.Fprintf(w, "  ✓ known_hosts pins of this server already current in %s\n", describePinRewrites(current))
			}
		}
	}

	replicas := max(target.Server.Replicas, 1)
	// What "bring it back" means depends on whether the database was
	// swapped: before, the previous database is untouched (a failed swap
	// puts it back itself); after, the restored one is in place.
	swapped := false
	stopped := func(err error) error {
		if !swapped {
			return fmt.Errorf("%w\nThe restore stopped with deploy/oberth scaled to 0 and the previous database in place. "+
				"Fix the cause and rerun oberth restore, or start the previous state again with: kubectl -n %s scale deploy/oberth --replicas=%d",
				err, cfg.Namespace, replicas)
		}
		return fmt.Errorf("%w\nThe restore stopped with deploy/oberth scaled to 0 and the RESTORED database already in place "+
			"(the previous files are in %s). Fix the cause and rerun oberth restore. "+
			"Scaling up now runs the restored database, which needs the restored compatibility values; "+
			"to return to the previous state instead, move those files back to /data first",
			err, target.previousFiles())
	}

	if parts.Secrets && !parts.History && archive.Grants.Present {
		if err := writeSecretAccess(ctx, deps.Deps, cfg.Namespace, archive.Grants); err != nil {
			return fmt.Errorf("restore database approvals: %w", err)
		}
		_, _ = fmt.Fprintf(w, "  ✓ restored database approvals (%d grants)\n", countGrants(archive.Grants))
	}
	_, _ = fmt.Fprintln(w, "Stopping the server...")
	if err := scaleServer(ctx, deps.Deps, cfg.Namespace, 0); err != nil {
		return err
	}
	if err := waitServerPodsGone(ctx, deps.Deps, cfg.Namespace, target.Server.Selector, cfg.Timeout); err != nil {
		return stopped(err)
	}

	if parts.History {
		_, _ = fmt.Fprintln(w, "Swapping the database on the volume...")
		if err := swapDatabase(ctx, cfg, deps, &archive, target, !parts.Secrets); err != nil {
			swapped = errors.Is(err, errRestoredDatabaseInPlace)
			return stopped(err)
		}
		swapped = true
		_, _ = fmt.Fprintf(w, "  ✓ restored database in place; the previous files are in %s\n", target.previousFiles())
	}

	// List again now that no server can submit new Workflows.
	if parts.History {
		workflows, err := listRunWorkflows(ctx, deps, cfg.ArgoNamespace)
		if err != nil {
			return stopped(fmt.Errorf("list Workflows for Terminate: %w", err))
		}
		orphans, err := orphanedWorkflows(ctx, workflows, archive, cfg.InterruptQueued)
		if err != nil {
			return stopped(err)
		}
		if err := terminateOrphanedWorkflows(ctx, deps, cfg.ArgoNamespace, orphans); err != nil {
			return stopped(err)
		}
		if len(orphans) > 0 {
			_, _ = fmt.Fprintf(w, "  ✓ requested Argo Terminate for %d orphaned workflows (exit handlers skipped; completed Workflows stay for TTL cleanup)\n", len(orphans))
		}
	}

	if len(helmArgs) > 0 {
		_, err := deps.RunHelm(ctx, helmArgs)
		if err != nil && strings.Contains(err.Error(), "conflict") && strings.Contains(err.Error(), ".spec.replicas") {
			// Helm 4 applies server-side; should the restore's own scaling
			// ever own .spec.replicas, take it back for Helm (#067).
			_, _ = fmt.Fprintln(w, "  Helm reported a field conflict on .spec.replicas from the restore's scaling; retrying with --force-conflicts")
			_, err = deps.RunHelm(ctx, append(slices.Clone(helmArgs), "--force-conflicts"))
		}
		if err != nil {
			return stopped(fmt.Errorf("set the restored values on release oberth: %w", err))
		}
		_, _ = fmt.Fprintln(w, "  ✓ release oberth updated with the restored values")
	}

	_, _ = fmt.Fprintln(w, "Starting the server...")
	if err := scaleServer(ctx, deps.Deps, cfg.Namespace, replicas); err != nil {
		return stopped(err)
	}
	if err := waitForPodReady(ctx, deps.Deps, cfg.Namespace, target.Server.Selector, cfg.Timeout); err != nil {
		_, _ = fmt.Fprintf(w, "  ⚠ the server is not ready yet (%v); check: kubectl -n %s logs deploy/oberth\n", err, cfg.Namespace)
		if parts.History {
			_, _ = fmt.Fprintf(w, "    If it cannot open or migrate the restored database, scale it to 0 and move the previous files back from %s to /data\n", target.previousFiles())
		}
	} else {
		_, _ = fmt.Fprintln(w, "  ✓ server ready")
		if pod, err := runningServerPod(ctx, deps.Deps, cfg.Namespace, target.Server.Selector); err == nil && pod != "" {
			target.DeployPublicKey = readRestoreDeployPublicKey(ctx, deps.Deps, cfg.Namespace)
			recordServerAudit(ctx, deps.Deps, cfg.Namespace, "restore.completed", map[string]any{
				"archive_sha256": archive.CipherSHA, "history": parts.History, "secrets": parts.Secrets,
				"from_audit_head_id": archive.restoredHeadID(), "from_audit_head_sha256": archive.restoredHeadSHA256(),
			})
		}
	}
	printRestoreNextSteps(w, cfg, archive, target, parts)
	return nil
}

// restoreHelmArgs builds the `helm upgrade --reuse-values` that sets the
// overlay, with the overlay in a values file inside the private working
// directory (removed when the restore returns). Retain the installed chart
// defaults deliberately; restore sets only database compatibility identities.
// When the target chart no longer declares Transit, remove its retired leaves
// from stored user values and explicitly retain installed compatibility identities.
func restoreHelmArgs(ctx context.Context, cfg RestoreConfig, deps backupDeps, target restoreTarget, work string) ([]string, error) {
	installed, err := readHelmValuesMap(ctx, deps.Deps, cfg.Namespace)
	if err != nil {
		return nil, err
	}
	if err := refuseRemovedReleaseValues(installed, "oberth restore"); err != nil {
		return nil, err
	}
	body, err := json.Marshal(target.Overlay)
	if err != nil {
		return nil, err
	}
	valuesFile := filepath.Join(work, "restore-values.json")
	if err := os.WriteFile(valuesFile, body, 0o600); err != nil { // #nosec G306 G703 -- fixed name in a fresh private directory; public values only.
		return nil, err
	}
	chart := strings.TrimSpace(cfg.ChartPath)
	args := []string{"upgrade", "oberth"}
	if chart == "" {
		if target.ChartVersion == "" {
			return nil, errors.New("cannot tell which chart version release oberth runs; pass --chart with the chart it was installed from")
		}
		// The chart is verified and written into the private working
		// directory now: the upgrade runs after the server is stopped and
		// the database swapped, too late to find a missing or forged chart.
		signed, err := verifySignedRelease(ctx, target.ChartVersion, cfg.Release, deps.Deps, work)
		if err != nil {
			return nil, fmt.Errorf("%w (or pass --chart with the chart release oberth was installed from)", err)
		}
		chart = signed.chartPath
	}
	args = append(args, chart)
	// Prove the chart resolves now, for the same reason.
	if _, err := deps.RunHelm(ctx, []string{"show", "chart", chart}); err != nil {
		return nil, fmt.Errorf("chart %s is not available (pass --chart with the chart release oberth was installed from): %w", args[2], err)
	}
	args = append(args, "-n", cfg.Namespace, "--reuse-values", "-f", valuesFile)
	targetDefaults, err := targetChartInstallerDefaults(ctx, deps.Deps, chart)
	if err != nil {
		return nil, err
	}
	return prepareRetiredInstallerReuse(ctx, deps.Deps, cfg.Namespace, work, args, targetDefaults)
}

// Preview the exact restore upgrade before either secrets or the database change.
func previewRestoreHelm(ctx context.Context, cfg RestoreConfig, deps backupDeps, target restoreTarget, work string) ([]string, error) {
	if len(target.Overlay) == 0 {
		return nil, nil
	}
	args, err := restoreHelmArgs(ctx, cfg, deps, target, work)
	if err != nil {
		return nil, err
	}
	preview := append(append([]string(nil), args...), "--dry-run=server", "--no-hooks", "--output=json")
	raw, err := deps.RunHelm(ctx, preview)
	clear(raw)
	if err != nil {
		return nil, fmt.Errorf("preview restore before writes: %w", err)
	}
	return args, nil
}

// scaleServer scales through the scale subresource. A merge patch of the
// Deployment made "oberth" the owner of .spec.replicas, and Helm 4's
// server-side apply then refused the next upgrade with a field conflict (#067).
func scaleServer(ctx context.Context, deps Deps, namespace string, replicas int32) error {
	patch := fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas)
	if _, err := deps.KubeClient.AppsV1().Deployments(namespace).Patch(ctx, "oberth", types.MergePatchType, []byte(patch), metav1.PatchOptions{}, "scale"); err != nil {
		return fmt.Errorf("scale deploy/oberth to %d: %w", replicas, err)
	}
	return nil
}

func waitServerPodsGone(ctx context.Context, deps Deps, namespace, selector string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		pods, err := deps.KubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return fmt.Errorf("list server pods: %w", err)
		}
		if len(pods.Items) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout (%s) waiting for the server pods to exit", timeout)
		}
		if err := sleepContext(ctx, pollInterval(deps)); err != nil {
			return err
		}
	}
}

// --- Swap through a helper pod ---

const restoreHelperContainer = "restore"

// restoreHelperPod uses the pinned utility image with the server's identity
// and placement. A separate scratch-compatible init command probes the server
// image schema; no shell or file tool ever runs in the server image.
func restoreHelperPod(name, namespace, timestamp string, server serverDeployment) *corev1.Pod {
	utilityImage := server.UtilityImage
	if utilityImage == "" {
		utilityImage = containerimage.Utility
	}
	nobody := int64(65534)
	podSecurity := &corev1.PodSecurityContext{}
	if server.PodSpec.SecurityContext != nil {
		podSecurity = server.PodSpec.SecurityContext.DeepCopy()
	}
	if podSecurity.RunAsUser == nil {
		podSecurity.RunAsUser = &nobody
	}
	if podSecurity.RunAsGroup == nil {
		podSecurity.RunAsGroup = &nobody
	}
	nonRoot, noEscalation, readOnly, noToken := true, false, true, false
	podSecurity.RunAsNonRoot = &nonRoot
	deadline := int64(3600)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app.kubernetes.io/name": "oberth-restore", "oberth.ci/restore": timestamp},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			ActiveDeadlineSeconds:        &deadline,
			AutomountServiceAccountToken: &noToken,
			SecurityContext:              podSecurity,
			ImagePullSecrets:             server.PodSpec.ImagePullSecrets,
			NodeSelector:                 server.PodSpec.NodeSelector,
			Tolerations:                  server.PodSpec.Tolerations,
			Affinity:                     server.PodSpec.Affinity,
			InitContainers:               []corev1.Container{{Name: "server-schema", Image: server.Image, Command: []string{"/usr/local/bin/oberth"}, Args: []string{"version", "--schema"}, SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &noEscalation, ReadOnlyRootFilesystem: &readOnly, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}}},
			Containers: []corev1.Container{{
				Name:            restoreHelperContainer,
				Image:           utilityImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"sleep", "3600"},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &noEscalation,
					ReadOnlyRootFilesystem:   &readOnly,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data"}},
			}},
			Volumes: []corev1.Volume{{
				Name:         "data",
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: server.ClaimName}},
			}},
		},
	}
}

func swapDatabase(ctx context.Context, cfg RestoreConfig, deps backupDeps, archive *restoreArchive, target restoreTarget, preserveGrants bool) (resultErr error) {
	name := "oberth-restore-" + strings.ToLower(target.Timestamp)
	server := target.Server
	if cfg.UtilityImage != "" {
		server.UtilityImage = cfg.UtilityImage
	}
	pod := restoreHelperPod(name, cfg.Namespace, target.Timestamp, server)
	pods := deps.KubeClient.CoreV1().Pods(cfg.Namespace)
	if _, err := pods.Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create restore pod: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		grace := int64(0)
		if err := pods.Delete(cleanupCtx, name, metav1.DeleteOptions{GracePeriodSeconds: &grace}); err != nil && !apierrors.IsNotFound(err) {
			resultErr = errors.Join(resultErr, fmt.Errorf("delete restore pod %s: %w", name, err))
		}
	}()
	if err := waitForPodRunning(ctx, deps.Deps, cfg.Namespace, "oberth.ci/restore="+target.Timestamp, cfg.Timeout); err != nil {
		return fmt.Errorf("wait for restore pod: %w", err)
	}

	if target.Schema == 0 {
		logArgs := []string{"logs", "-n", cfg.Namespace, name, "-c", "server-schema"}
		if deps.ContextName != "" {
			logArgs = append([]string{"--context", deps.ContextName}, logArgs...)
		}
		out, err := deps.RunCommand(ctx, nil, "kubectl", logArgs...)
		if err != nil {
			return fmt.Errorf("read restore schema init log: %w", err)
		}
		schema, err := parseUtilitySchema(out)
		if err != nil {
			return err
		}
		if schema < archive.Manifest.Database.Schema {
			return fmt.Errorf("the target server image writes schema %d but the backup holds schema %d; upgrade the target first", schema, archive.Manifest.Database.Schema)
		}
		output := deps.Output
		if output == nil {
			output = io.Discard
		}
		if err := preflightRestoreMigration(ctx, *archive, schema, target.previousFiles(), output); err != nil {
			return err
		}
	}

	inPod := func(stdin []byte, script string, args ...string) ([]byte, error) {
		command := append([]string{"sh", "-c", script, "sh"}, args...)
		return deps.RunCommand(ctx, stdin, "kubectl", kubectlExecArgs(deps.ContextName, cfg.Namespace, name, restoreHelperContainer, stdin != nil, command...)...)
	}
	if preserveGrants {
		if err := retainStoppedTargetGrants(ctx, cfg, deps, archive, target, name); err != nil {
			return err
		}
	}
	if out, err := inPod(nil, stageResetScript); err != nil {
		return fmt.Errorf("clear staging directory %s: %w%s", restoreStageDir, err, commandOutputSuffix(out))
	}
	upload := func(path, member string) error {
		if deps.runIO == nil {
			return errors.New("restore stdin stream runner unavailable")
		}
		file, err := os.Open(path) // #nosec G304 -- verified member in private restore working directory.
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		info, err := file.Stat()
		if err != nil {
			return err
		}
		command := []string{"sh", "-c", stageUploadScript, "sh", member}
		out := &backupDiagnostic{}
		err = deps.runIO(ctx, file, out, "kubectl", kubectlExecArgs(deps.ContextName, cfg.Namespace, name, restoreHelperContainer, true, command...)...)
		if err != nil {
			return fmt.Errorf("upload %s: %w%s", member, err, commandOutputSuffix(out.data))
		}
		return checkUploadedSize(member, out.data, info.Size())
	}
	if err := upload(archive.Database.Main, "oberth.sqlite"); err != nil {
		return err
	}
	walSHA := "-"
	if archive.Database.WAL != "" {
		walSHA = archive.Database.WALSHA256
		if err := upload(archive.Database.WAL, "oberth.sqlite-wal"); err != nil {
			return err
		}
	}
	out, err := inPod(nil, swapScript, target.Timestamp, archive.Database.MainSHA256, walSHA)
	if err != nil {
		if bytes.Contains(out, []byte(swapCompleteMarker)) {
			return fmt.Errorf("%w: %w%s", errRestoredDatabaseInPlace, err, commandOutputSuffix(out))
		}
		return fmt.Errorf("swap database files: %w%s", err, commandOutputSuffix(out))
	}
	want := fmt.Sprintf("%d:%d 600", *pod.Spec.SecurityContext.RunAsUser, *pod.Spec.SecurityContext.RunAsGroup)
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if got := strings.TrimSpace(lines[len(lines)-1]); got != want {
		return fmt.Errorf("%w: restored database has owner and mode %q, want %q", errRestoredDatabaseInPlace, got, want)
	}
	if len(target.RestoredRunning) > 0 {
		marker, err := json.Marshal(map[string][]string{"runs": target.RestoredRunning})
		if err != nil {
			return err
		}
		if out, err := inPod(marker, restoredRunsScript); err != nil {
			return fmt.Errorf("%w: write the restored-runs marker (without it the server would re-execute %d runs): %w%s",
				errRestoredDatabaseInPlace, len(target.RestoredRunning), err, commandOutputSuffix(out))
		}
	}
	return nil
}

// restoredRunsScript writes the server's restored-runs marker
// (service.RestoredRunsFile) from stdin, atomically, beside the database.
const restoredRunsScript = `set -eu
umask 077
cat > /data/.oberth-restored-runs.json.tmp
mv -f /data/.oberth-restored-runs.json.tmp /data/` + restoredRunsFile

// restoredRunsFile is service.RestoredRunsFile (a test keeps them equal).
const restoredRunsFile = ".oberth-restored-runs.json"

// restoredRunningRuns lists the runs the archive records as running with a
// Workflow: on the target their Workflows do not exist, and the server marks
// them interrupted from the marker instead of re-executing them. With
// queued (a rollback restore) it lists every active run: queued or waiting
// work in a pre-upgrade backup may already have run after the upgrade, and
// the server cancels it instead of dispatching it again.
func restoredRunningRuns(ctx context.Context, archive restoreArchive, queued bool) (ids []string, resultErr error) {
	compat := archive.Manifest.Compatibility
	inspection, err := store.InspectCurrent(ctx, archive.LocalDatabase, store.Options{Protocol: store.ProtocolConfig{SchemaIdentity: compat.SchemaIdentity, AuditDomain: compat.AuditDomain}, SnapshotTempDir: filepath.Dir(archive.LocalDatabase)})
	if err != nil {
		return nil, fmt.Errorf("open restored database: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, inspection.Close()) }()
	if queued {
		if ids, err = inspection.ActiveRunIDs(ctx); err != nil {
			return nil, fmt.Errorf("list the backup's active runs: %w", err)
		}
		return ids, nil
	}
	runs, err := inspection.RunningRunsWithJobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list the backup's running runs: %w", err)
	}
	for _, run := range runs {
		ids = append(ids, run.ID)
	}
	return ids, nil
}

// checkUploadedSize compares the size stageUploadScript reports with what
// was sent, so a truncated upload stops before the swap (#067).
func checkUploadedSize(member string, out []byte, want int64) error {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	got, err := strconv.ParseInt(strings.TrimSpace(lines[len(lines)-1]), 10, 64)
	if err != nil {
		return fmt.Errorf("upload %s: the restore pod did not report the received size%s", member, commandOutputSuffix(out))
	}
	if got != want {
		return fmt.Errorf("upload %s: the restore pod received %d of %d bytes; nothing was swapped, rerun oberth restore", member, got, want)
	}
	return nil
}

func writeSecretAccess(ctx context.Context, deps Deps, namespace string, grants secretAccessExport) error {
	if !grants.Present {
		return nil
	}
	if err := validateExportedGrants(grants.Grants); err != nil {
		return err
	}
	current, err := readSecretAccess(ctx, deps, namespace)
	if err != nil {
		return err
	}
	desired := map[model.SecretAccessKey]bool{}
	for _, key := range grants.Grants {
		desired[key] = true
	}
	active := map[model.SecretAccessKey]bool{}
	for _, key := range current.Grants {
		active[key] = true
	}
	// Remove excess authority before adding the exact restored approvals.
	for _, key := range current.Grants {
		if !desired[key] {
			if err := CallAdmin(ctx, Config{Namespace: namespace}, deps, "access-revoke", key, nil); err != nil {
				return err
			}
		}
	}
	for _, key := range grants.Grants {
		if !active[key] {
			if err := CallAdmin(ctx, Config{Namespace: namespace}, deps, "access-allow", key, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- Orphaned Argo Workflows ---

// runWorkflow is one Argo Workflow submitted for an Oberth run.
type runWorkflow struct {
	Name            string
	RunID           string
	UID             string
	ResourceVersion string
	Phase           string
	Shutdown        string
}

func listRunWorkflows(ctx context.Context, deps backupDeps, namespace string) ([]runWorkflow, error) {
	args := kubectlContextArgs(deps.ContextName, "get", "workflows.argoproj.io", "-n", namespace, "-l", "oberth.ci/run", "-o", "json")
	out, err := deps.runStdout(ctx, nil, "kubectl", args...)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name            string            `json:"name"`
				UID             string            `json:"uid"`
				ResourceVersion string            `json:"resourceVersion"`
				Labels          map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				Shutdown string `json:"shutdown"`
			} `json:"spec"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("parse workflow list: %w", err)
	}
	workflows := make([]runWorkflow, 0, len(list.Items))
	for _, item := range list.Items {
		workflows = append(workflows, runWorkflow{Name: item.Metadata.Name, RunID: item.Metadata.Labels["oberth.ci/run"], UID: item.Metadata.UID, ResourceVersion: item.Metadata.ResourceVersion, Phase: item.Status.Phase, Shutdown: item.Spec.Shutdown})
	}
	return workflows, nil
}

// orphanedWorkflows returns the workflows whose run the restored database
// does not know: submitted by the database being replaced, they would hold
// pipeline slots and report to runs that no longer exist (issue 048). With
// newer (a rollback restore) it also returns the workflows the restored
// database does not name for their run: submitted after the backup for a run
// it holds as queued (or under another name), they are work of the version
// the rollback undoes, and the restored run is cancelled, not continued.
func orphanedWorkflows(ctx context.Context, workflows []runWorkflow, archive restoreArchive, newer bool) (orphans []runWorkflow, resultErr error) {
	if len(workflows) == 0 {
		return nil, nil
	}
	compat := archive.Manifest.Compatibility
	inspection, err := store.InspectCurrent(ctx, archive.LocalDatabase, store.Options{Protocol: store.ProtocolConfig{SchemaIdentity: compat.SchemaIdentity, AuditDomain: compat.AuditDomain}, SnapshotTempDir: filepath.Dir(archive.LocalDatabase)})
	if err != nil {
		return nil, fmt.Errorf("open restored database: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, inspection.Close()) }()
	ids := make([]string, 0, len(workflows))
	for _, wf := range workflows {
		ids = append(ids, wf.RunID)
	}
	known, err := inspection.RunIDsExist(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, wf := range workflows {
		if !known[wf.RunID] {
			orphans = append(orphans, wf)
			continue
		}
		if newer {
			name, err := inspection.RunWorkflowName(ctx, wf.RunID)
			if err != nil {
				return nil, fmt.Errorf("read restored run %s: %w", wf.RunID, err)
			}
			if name != wf.Name {
				orphans = append(orphans, wf)
			}
		}
	}
	return orphans, nil
}

// terminateOrphanedWorkflows uses native immediate shutdown because restored
// history has no owner that can safely supply credentials to their exit Pods.
// UID fences replacement; conflicts trigger a bounded identity re-read.
func terminateOrphanedWorkflows(ctx context.Context, deps backupDeps, namespace string, workflows []runWorkflow) error {
	for _, wf := range workflows {
		if wf.Phase == "Succeeded" || wf.Phase == "Failed" || wf.Phase == "Error" || wf.Shutdown == "Terminate" {
			continue
		}
		if wf.Name == "" || wf.RunID == "" || wf.UID == "" {
			return fmt.Errorf("terminate orphaned Workflow %s: original object identity required", wf.Name)
		}
		patch, err := json.Marshal(map[string]any{"metadata": map[string]string{"uid": wf.UID}, "spec": map[string]string{"shutdown": "Terminate"}})
		if err != nil {
			return err
		}
		for attempt := 0; attempt < 3; attempt++ {
			args := kubectlContextArgs(deps.ContextName, "patch", "workflows.argoproj.io", wf.Name, "-n", namespace, "--type=merge", "-p", string(patch))
			output, err := deps.RunCommand(ctx, nil, "kubectl", args...)
			if err == nil {
				break
			}
			if attempt == 2 || (!apierrors.IsConflict(err) && !strings.Contains(string(output), "(Conflict)")) {
				return fmt.Errorf("terminate orphaned Workflow %s: %w%s", wf.Name, err, commandOutputSuffix(output))
			}
			listed, readErr := listRunWorkflows(ctx, deps, namespace)
			if readErr != nil {
				return readErr
			}
			found := false
			for _, current := range listed {
				if current.Name != wf.Name {
					continue
				}
				found = true
				if current.UID != wf.UID || current.RunID != wf.RunID {
					return fmt.Errorf("terminate orphaned Workflow %s: original object was replaced", wf.Name)
				}
				if current.Phase == "Succeeded" || current.Phase == "Failed" || current.Phase == "Error" || current.Shutdown == "Terminate" {
					found = false
				}
				break
			}
			if !found {
				break
			}
		}
	}
	return nil
}

// kubectlContextArgs prefixes a kubectl argv with --context when selected.
func kubectlContextArgs(contextName string, args ...string) []string {
	if contextName == "" {
		return args
	}
	return append([]string{"--context", contextName}, args...)
}

func printRestoreNextSteps(w io.Writer, cfg RestoreConfig, archive restoreArchive, target restoreTarget, parts restoreParts) {
	_, _ = fmt.Fprintln(w, "\nRestore complete. Next steps:")
	step := 0
	next := func(format string, args ...any) {
		step++
		_, _ = fmt.Fprintf(w, "  %d. "+format+"\n", append([]any{step}, args...)...)
	}
	if parts.Secrets {
		next("Recreate the per-repository identities pipelines read secrets with, from the restored grants\n" +
			"     (keep your usual install flags):\n" +
			"         read -rs BAO_TOKEN && export BAO_TOKEN\n" +
			"         oberth install --install-secretstore --upgrade")
	}
	next("Re-apply your own Helm values files, if you use any: oberth upgrade -f FILE")
	if parts.History {
		next("Uplinks and their tokens are the backup's again: a token this install printed no longer works; the previous ones do.")
	}
	printRestoreDeployKey(w, cfg, target)
	next("This install keeps its own server CA and SSH host key: other machines re-pin ~/.config/oberth/ca.crt and\n" +
		"     drop a stale known_hosts entry (ssh-keygen -R '[<host>]:30022').")
	if parts.Secrets && len(target.Pins.OldCA) > 0 {
		next("These restored secrets still hold the backup's server CA, which this install replaced; put this install's CA\n"+
			"     (~/.config/oberth/ca.crt after oberth install) in them: %s", strings.Join(target.Pins.OldCA, ", "))
	}
	if parts.Secrets {
		for _, left := range []struct {
			findings []pinFinding
			why      string
		}{
			{target.Pins.Mixed, "list this server's SSH host among other hosts on one known_hosts line"},
			{target.Pins.Loopback, "pin a loopback name of this server, which a pipeline resolves to another machine"},
			{target.Pins.OldKey, "pin the backup's SSH host key under names this restore cannot attribute to this server"},
		} {
			if len(left.findings) > 0 {
				next("These restored secrets %s; they were left as they were,\n"+
					"     check them and update the key by hand where it means this server: %s", left.why, describePinFindings(left.findings))
			}
		}
	}
	next("If this node keeps OpenBao unsealed with deploy/systemd/oberth-openbao-unseal, bind this install's recovery file to the host TPM2 as documented in deploy/systemd/README.md:\n" +
		"         sudo oberth unseal --secrets-file <root-owned-recovery-file> --seal-to-host <new-credential-file> --yes\n" +
		"         update LoadCredentialEncrypted to that new file, then run sudo systemctl daemon-reload\n" +
		"         sudo systemctl restart oberth-openbao-unseal")
}

// errRestoredDatabaseInPlace marks a swap failure that happened after the
// restored database was already in place.
var errRestoredDatabaseInPlace = errors.New("the restored database is in place but its final check failed")
