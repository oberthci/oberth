package installer

// `oberth restore`: rebuild an installation's state from an `oberth backup`
// archive onto a (typically fresh) install. It automates the hand restore of
// tuxbox on 2026-10-16 (issues 038 and 048): validate the database offline,
// write OpenBao state field-for-field and verify it by read-back, scale the
// server to zero, swap the database on its volume keeping the previous one
// aside, restore the secret-access table, remove Argo Workflows whose runs
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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/internal/auditanchor"
	"github.com/oberthci/oberth/internal/store"
)

// RestoreConfig selects the archive, its key, and the target installation.
type RestoreConfig struct {
	KubeconfigPath   string
	ContextName      string
	Namespace        string
	OpenBaoNamespace string
	ArgoNamespace    string
	Input            string
	PrivateKey       string
	// ChartPath is the local chart used when compatibility values must be
	// set on the release; empty selects the published chart at the version
	// the release already runs.
	ChartPath string
	// Only limits the restore to these parts (RestorePartHistory,
	// RestorePartSecrets); empty restores everything the archive holds.
	Only []string
	// Inspect prints what the archive holds and stops; no cluster is used.
	Inspect bool
	DryRun  bool
	Yes     bool
	Timeout time.Duration
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
	if archive.Manifest, err = openBackupArchive(ctx, cfg.Input, deps.cipher, work); err != nil {
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
			{backupMemberSecretAccess, &archive.Grants},
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
	if archive.Report, err = verifyDatabaseArchive(ctx, databaseMember, archive.Manifest.Database.Schema, protocol); err != nil {
		return archive, err
	}
	if archive.Report.AuditHead.ID != archive.Manifest.Database.AuditHeadID ||
		hex.EncodeToString(archive.Report.AuditHead.SHA256) != archive.Manifest.Database.AuditHeadSHA256 {
		return archive, errors.New("the verified database audit head differs from the one the manifest records")
	}
	if archive.Database, err = extractDatabaseFiles(databaseMember); err != nil {
		return archive, err
	}
	if archive.LocalDatabase, err = writeRestoredDatabase(work, archive.Database); err != nil {
		return archive, err
	}
	if archive.Contents, err = summarizeDatabase(ctx, archive.LocalDatabase, protocol); err != nil {
		return archive, err
	}
	_, _ = fmt.Fprintf(deps.Output, "Verified: all %d parts match the manifest; the database (schema %d) passes integrity, foreign-key and audit-chain checks offline.\n",
		len(archive.Manifest.Members), archive.Report.Schema)
	return archive, nil
}

// restoreTarget is what the plan learned about the target installation.
type restoreTarget struct {
	Server       serverDeployment
	Pod          string
	ChartVersion string
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
	// KV is the archive's secrets as they will be written: known_hosts pins
	// of this server updated (#074).
	KV   kvExport
	Pins serverPins
}

func runRestore(ctx context.Context, cfg RestoreConfig, deps backupDeps) (resultErr error) {
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
	release, found := findHelmRelease(ctx, deps.Deps, "oberth", cfg.Namespace)
	if !found {
		return target, openBaoExec{}, fmt.Errorf("no oberth Helm release in namespace %s; install Oberth on the target first (oberth setup)", cfg.Namespace)
	}
	target.ChartVersion = chartVersionFromRelease(release.Chart, "oberth")
	if target.Pod, err = runningServerPod(ctx, deps.Deps, cfg.Namespace, target.Server.Selector); err != nil {
		return target, openBaoExec{}, err
	}
	backupSchema := archive.Manifest.Database.Schema
	if target.Pod != "" {
		if target.Schema, err = podSchemaVersion(ctx, deps.RunCommand, deps.ContextName, cfg.Namespace, target.Pod, serverContainerName); err != nil {
			return target, openBaoExec{}, err
		}
		if parts.History && target.Schema < backupSchema {
			return target, openBaoExec{}, fmt.Errorf("the target server writes schema %d but the backup holds schema %d; upgrade the target to a release at least as new as %s first",
				target.Schema, backupSchema, displayOr(archive.Manifest.Source.ServerVersion, "the source"))
		}
	}
	target.ClusterUID = clusterUID(ctx, deps)

	if parts.History {
		readTargetContents(ctx, cfg, deps, &target, work)
		target.Overlay = compatibilityOverlay(archive, target)
		workflows, listErr := listRunWorkflows(ctx, deps, cfg.ArgoNamespace)
		if listErr != nil {
			target.Notes = append(target.Notes, fmt.Sprintf("could not list Argo Workflows in %s (%v); orphaned workflows are not cleaned up", cfg.ArgoNamespace, listErr))
		} else if target.Orphans, err = orphanedWorkflows(ctx, workflows, archive); err != nil {
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
	if _, err := snapshotServerDatabase(ctx, deps, cfg.Namespace, target.Pod, target.Schema, protocol, dir, snapshot); err != nil {
		target.ContentsNote = err.Error()
		return
	}
	summary, err := summarizeDatabaseArchive(ctx, snapshot, dir, protocol)
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

func countGrants(grants secretAccessExport) int {
	var entries []grantEntry
	if err := yaml.Unmarshal([]byte(grants.Data[secretAccessConfigMapKey]), &entries); err != nil {
		return 0
	}
	return len(entries)
}

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
	var helmArgs []string
	if len(target.Overlay) > 0 {
		var err error
		if helmArgs, err = restoreHelmArgs(ctx, cfg, deps, target, work); err != nil {
			return err
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
		if len(target.Pins.Rewritten) > 0 {
			_, _ = fmt.Fprintf(w, "  ✓ known_hosts pins of this server updated to its SSH host key in %s\n", describePinRewrites(target.Pins.Rewritten))
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
			"(the previous files are in /data/pre-restore-%s/ on PVC %s). Fix the cause and rerun oberth restore. "+
			"Scaling up now runs the restored database, which needs the restored compatibility values; "+
			"to return to the previous state instead, move those files back to /data first",
			err, target.Timestamp, target.Server.ClaimName)
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
		if err := swapDatabase(ctx, cfg, deps, archive, target); err != nil {
			swapped = errors.Is(err, errRestoredDatabaseInPlace)
			return stopped(err)
		}
		swapped = true
		_, _ = fmt.Fprintf(w, "  ✓ restored database in place; the previous files are in /data/pre-restore-%s/ on PVC %s\n", target.Timestamp, target.Server.ClaimName)
	}

	if parts.Secrets && archive.Grants.Present {
		if err := writeSecretAccess(ctx, deps.Deps, cfg.Namespace, archive.Grants); err != nil {
			return stopped(err)
		}
		_, _ = fmt.Fprintf(w, "  ✓ ConfigMap %s restored (%d grants)\n", secretAccessConfigMapName, countGrants(archive.Grants))
	}

	// List again now that no server can submit new workflows.
	if workflows, err := listRunWorkflows(ctx, deps, cfg.ArgoNamespace); parts.History && err == nil {
		orphans, err := orphanedWorkflows(ctx, workflows, archive)
		if err != nil {
			return stopped(err)
		}
		if err := deleteWorkflows(ctx, deps, cfg.ArgoNamespace, orphans); err != nil {
			return stopped(err)
		}
		if len(orphans) > 0 {
			_, _ = fmt.Fprintf(w, "  ✓ deleted %d orphaned workflows\n", len(orphans))
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
	} else {
		_, _ = fmt.Fprintln(w, "  ✓ server ready")
	}
	printRestoreNextSteps(w, cfg, archive, target, parts)
	return nil
}

// restoreHelmArgs builds the `helm upgrade --reuse-values` that sets the
// overlay, with the overlay in a values file inside the private working
// directory (removed when the restore returns).
func restoreHelmArgs(ctx context.Context, cfg RestoreConfig, deps backupDeps, target restoreTarget, work string) ([]string, error) {
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
		if err := refreshHelmRepo(ctx, deps.Deps, oberthRepoName, OberthHelmRepoURL); err != nil {
			return nil, fmt.Errorf("refresh the Oberth chart repository (or pass --chart): %w", err)
		}
		args = append(args, oberthRepoName+"/oberth", "--version", helmChartVersion(target.ChartVersion))
	} else {
		args = append(args, chart)
	}
	// Prove the chart resolves now: the upgrade runs after the server is
	// stopped and the database swapped, too late to find a missing chart.
	show := []string{"show", "chart", args[2]}
	if chart == "" {
		show = append(show, "--version", helmChartVersion(target.ChartVersion))
	}
	if _, err := deps.RunHelm(ctx, show); err != nil {
		return nil, fmt.Errorf("chart %s is not available (pass --chart with the chart release oberth was installed from): %w", args[2], err)
	}
	return append(args, "-n", cfg.Namespace, "--reuse-values", "-f", valuesFile), nil
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

// restoreHelperPod mounts the server's claim with the server's own image,
// identity and placement, so files it writes carry the server's owner and it
// schedules wherever the volume can attach.
func restoreHelperPod(name, namespace, timestamp string, server serverDeployment) *corev1.Pod {
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
			Containers: []corev1.Container{{
				Name:            restoreHelperContainer,
				Image:           server.Image,
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

func swapDatabase(ctx context.Context, cfg RestoreConfig, deps backupDeps, archive restoreArchive, target restoreTarget) (resultErr error) {
	name := "oberth-restore-" + strings.ToLower(target.Timestamp)
	pod := restoreHelperPod(name, cfg.Namespace, target.Timestamp, target.Server)
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
		schema, err := podSchemaVersion(ctx, deps.RunCommand, deps.ContextName, cfg.Namespace, name, restoreHelperContainer)
		if err != nil {
			return err
		}
		if schema < archive.Manifest.Database.Schema {
			return fmt.Errorf("the target server image writes schema %d but the backup holds schema %d; upgrade the target first", schema, archive.Manifest.Database.Schema)
		}
	}

	inPod := func(stdin []byte, script string, args ...string) ([]byte, error) {
		command := append([]string{"sh", "-c", script, "sh"}, args...)
		return deps.RunCommand(ctx, stdin, "kubectl", kubectlExecArgs(deps.ContextName, cfg.Namespace, name, restoreHelperContainer, stdin != nil, command...)...)
	}
	if out, err := inPod(nil, stageResetScript); err != nil {
		return fmt.Errorf("clear staging directory %s: %w%s", restoreStageDir, err, commandOutputSuffix(out))
	}
	upload := func(data []byte, member string) error {
		out, err := inPod(data, stageUploadScript, member)
		if err != nil {
			return fmt.Errorf("upload %s: %w%s", member, err, commandOutputSuffix(out))
		}
		return checkUploadedSize(member, out, len(data))
	}
	if err := upload(archive.Database.Main, "oberth.sqlite"); err != nil {
		return err
	}
	walSHA := "-"
	if archive.Database.WAL != nil {
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
	return nil
}

// checkUploadedSize compares the size stageUploadScript reports with what
// was sent, so a truncated upload stops before the swap (#067).
func checkUploadedSize(member string, out []byte, want int) error {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	got, err := strconv.Atoi(strings.TrimSpace(lines[len(lines)-1]))
	if err != nil {
		return fmt.Errorf("upload %s: the restore pod did not report the received size%s", member, commandOutputSuffix(out))
	}
	if got != want {
		return fmt.Errorf("upload %s: the restore pod received %d of %d bytes; nothing was swapped, rerun oberth restore", member, got, want)
	}
	return nil
}

func writeSecretAccess(ctx context.Context, deps Deps, namespace string, grants secretAccessExport) error {
	configMaps := deps.KubeClient.CoreV1().ConfigMaps(namespace)
	existing, err := configMaps.Get(ctx, secretAccessConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = configMaps.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: secretAccessConfigMapName, Namespace: namespace},
			Data:       grants.Data,
		}, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create ConfigMap %s: %w", secretAccessConfigMapName, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get ConfigMap %s: %w", secretAccessConfigMapName, err)
	}
	existing.Data = grants.Data
	if _, err := configMaps.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update ConfigMap %s: %w", secretAccessConfigMapName, err)
	}
	return nil
}

// --- Orphaned Argo Workflows ---

// runWorkflow is one Argo Workflow submitted for an Oberth run.
type runWorkflow struct {
	Name  string
	RunID string
	Phase string
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
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
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
		workflows = append(workflows, runWorkflow{Name: item.Metadata.Name, RunID: item.Metadata.Labels["oberth.ci/run"], Phase: item.Status.Phase})
	}
	return workflows, nil
}

// orphanedWorkflows returns the workflows whose run the restored database
// does not know: submitted by the database being replaced, they would hold
// pipeline slots and report to runs that no longer exist (issue 048).
func orphanedWorkflows(ctx context.Context, workflows []runWorkflow, archive restoreArchive) (orphans []runWorkflow, resultErr error) {
	if len(workflows) == 0 {
		return nil, nil
	}
	compat := archive.Manifest.Compatibility
	inspection, err := store.InspectCurrent(ctx, archive.LocalDatabase, store.Options{Protocol: store.ProtocolConfig{SchemaIdentity: compat.SchemaIdentity, AuditDomain: compat.AuditDomain}})
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
		}
	}
	return orphans, nil
}

func deleteWorkflows(ctx context.Context, deps backupDeps, namespace string, workflows []runWorkflow) error {
	if len(workflows) == 0 {
		return nil
	}
	args := kubectlContextArgs(deps.ContextName, "delete", "workflows.argoproj.io", "-n", namespace, "--ignore-not-found", "--wait=false")
	for _, wf := range workflows {
		args = append(args, wf.Name)
	}
	if out, err := deps.RunCommand(ctx, nil, "kubectl", args...); err != nil {
		return fmt.Errorf("delete orphaned workflows: %w%s", err, commandOutputSuffix(out))
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
	next("This install keeps its own server CA and SSH host key: other machines re-pin ~/.config/oberth/ca.crt and\n" +
		"     drop a stale known_hosts entry (ssh-keygen -R '[<host>]:30022').")
	if parts.Secrets && len(target.Pins.OldCA) > 0 {
		next("These restored secrets still hold the backup's server CA, which this install replaced; put this install's CA\n"+
			"     (~/.config/oberth/ca.crt after oberth install) in them: %s", strings.Join(target.Pins.OldCA, ", "))
	}
	if parts.Secrets && len(target.Pins.Mixed) > 0 {
		next("These restored secrets list this server's SSH host among other hosts on one known_hosts line, left as they were;\n"+
			"     update the key for this server by hand: %s", strings.Join(target.Pins.Mixed, ", "))
	}
	next("If this node keeps OpenBao unsealed with deploy/systemd/oberth-openbao-unseal, give it this install's unseal key:\n" +
		"         sudo install -m 0400 -o root -g root <unseal-key-file> /etc/oberth/openbao-unseal-key\n" +
		"         sudo systemctl restart oberth-openbao-unseal")
}

// errRestoredDatabaseInPlace marks a swap failure that happened after the
// restored database was already in place.
var errRestoredDatabaseInPlace = errors.New("the restored database is in place but its final check failed")
