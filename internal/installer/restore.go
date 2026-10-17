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
	"sort"
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
	DryRun    bool
	Yes       bool
	Timeout   time.Duration
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
	deps, err := prepareOperatorDeps(host, cfg.KubeconfigPath, cfg.ContextName, !cfg.DryRun)
	if err != nil {
		return err
	}
	deps.cipher = opensslCMS{binary: deps.openssl, privateKey: cfg.PrivateKey}
	if deps.rootToken == "" && !cfg.DryRun {
		return missingRootTokenError("restore")
	}
	return runRestore(ctx, cfg, deps)
}

// restoreArchive is a decrypted, verified archive ready to apply.
type restoreArchive struct {
	Manifest  backupManifest
	CipherSHA string
	KV        kvExport
	Policies  []policyExport
	Roles     rolesExport
	Grants    secretAccessExport
	Values    map[string]any
	Report    store.BackupReport
	Database  restoredDatabase
	// LocalDatabase is the verified pair written for local inspection.
	LocalDatabase string
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
	for _, member := range []struct {
		name  string
		value any
	}{
		{backupMemberKV, &archive.KV},
		{backupMemberPolicies, &archive.Policies},
		{backupMemberRoles, &archive.Roles},
		{backupMemberSecretAccess, &archive.Grants},
		{backupMemberHelmValues, &archive.Values},
	} {
		if err := readBackupJSON(work, member.name, member.value); err != nil {
			return archive, err
		}
	}
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
	_, _ = fmt.Fprintf(deps.Output, "Archive verified: %d members match the manifest; database schema %d, audit head %d, integrity, foreign keys,\n"+
		"  schema identity %s and the audit chain under domain %s all check out offline.\n",
		len(archive.Manifest.Members), archive.Report.Schema, archive.Report.AuditHead.ID, compat.SchemaIdentity, compat.AuditDomain)
	return archive, nil
}

// restoreTarget is what the plan learned about the target installation.
type restoreTarget struct {
	Server       serverDeployment
	Pod          string
	ChartVersion string
	Values       map[string]any
	// Schema is the target server's schema version, 0 when no server pod
	// is running to ask (it is then checked in the helper pod).
	Schema    int
	OpenBao   openBaoRestorePlan
	Overlay   map[string]any
	Notes     []string
	Orphans   []runWorkflow
	Timestamp string
}

func runRestore(ctx context.Context, cfg RestoreConfig, deps backupDeps) (resultErr error) {
	w := deps.Output
	work, err := os.MkdirTemp("", "oberth-restore-")
	if err != nil {
		return fmt.Errorf("create private working directory: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(work)) }()

	_, _ = fmt.Fprintf(w, "Decrypting and verifying %s...\n", cfg.Input)
	archive, err := loadRestoreArchive(ctx, cfg, deps, work)
	if err != nil {
		return err
	}

	target, bao, err := inspectRestoreTarget(ctx, cfg, deps, archive)
	if err != nil {
		return err
	}
	printRestorePlan(w, cfg, deps, archive, target)
	if cfg.DryRun {
		_, _ = fmt.Fprintln(w, "\nNo changes were made (--dry-run).")
		return nil
	}
	if err := confirmRestore(cfg, deps); err != nil {
		return err
	}
	return applyRestore(ctx, cfg, deps, archive, target, bao, work)
}

// inspectRestoreTarget reads (never writes) the target installation.
func inspectRestoreTarget(ctx context.Context, cfg RestoreConfig, deps backupDeps, archive restoreArchive) (restoreTarget, openBaoExec, error) {
	target := restoreTarget{Timestamp: deps.now().UTC().Format("20060102T150405Z")}
	var err error
	if target.Server, err = inspectServerDeployment(ctx, deps.Deps, cfg.Namespace); err != nil {
		return target, openBaoExec{}, err
	}
	release, found := findHelmRelease(ctx, deps.Deps, "oberth", cfg.Namespace)
	if !found {
		return target, openBaoExec{}, fmt.Errorf("no oberth Helm release in namespace %s; install Oberth on the target first (oberth install --install-secretstore)", cfg.Namespace)
	}
	target.ChartVersion = chartVersionFromRelease(release.Chart, "oberth")
	raw, err := releaseValues(ctx, deps.Deps, cfg.Namespace)
	if err != nil {
		return target, openBaoExec{}, err
	}
	if err := json.Unmarshal(raw, &target.Values); err != nil {
		return target, openBaoExec{}, fmt.Errorf("parse target release values: %w", err)
	}
	if target.Pod, err = runningServerPod(ctx, deps.Deps, cfg.Namespace, target.Server.Selector); err != nil {
		return target, openBaoExec{}, err
	}
	backupSchema := archive.Manifest.Database.Schema
	if target.Pod != "" {
		if target.Schema, err = podSchemaVersion(ctx, deps.RunCommand, deps.ContextName, cfg.Namespace, target.Pod, serverContainerName); err != nil {
			return target, openBaoExec{}, err
		}
		if target.Schema < backupSchema {
			return target, openBaoExec{}, fmt.Errorf("the target server writes schema %d but the backup holds schema %d; upgrade the target to a release at least as new as %s first",
				target.Schema, backupSchema, displayOr(archive.Manifest.Source.ServerVersion, "the source"))
		}
	}

	bao, err := connectOpenBao(ctx, deps.Deps, cfg.OpenBaoNamespace)
	if err != nil {
		return target, openBaoExec{}, err
	}
	if deps.rootToken != "" {
		if target.OpenBao, err = planOpenBaoRestore(ctx, bao, deps.rootToken, archive.KV, archive.Policies, archive.Roles); err != nil {
			return target, openBaoExec{}, err
		}
	}

	target.Overlay, target.Notes = restoreValuesOverlay(archive, target, cfg.ArgoNamespace)

	workflows, listErr := listRunWorkflows(ctx, deps, cfg.ArgoNamespace)
	if listErr != nil {
		target.Notes = append(target.Notes, fmt.Sprintf("could not list Argo Workflows in %s (%v); orphaned workflows are not cleaned up", cfg.ArgoNamespace, listErr))
	} else if target.Orphans, err = orphanedWorkflows(ctx, workflows, archive); err != nil {
		return target, openBaoExec{}, err
	}
	return target, bao, nil
}

func displayOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// restoreValuesOverlay returns the Helm values a restore sets on the target
// release — the compatibility values the restored database must be opened
// under and, when recorded, argo.releaseWIF — plus notes about recorded
// values that are deliberately not reapplied.
func restoreValuesOverlay(archive restoreArchive, target restoreTarget, argoNamespace string) (map[string]any, []string) {
	overlay := map[string]any{}
	var notes []string
	want := archive.Manifest.Compatibility
	if target.Server.Compatibility != want {
		overlay["compatibility"] = map[string]any{
			"schemaIdentity": want.SchemaIdentity,
			"auditDomain":    want.AuditDomain,
			"witnessKeyInfo": want.WitnessKeyInfo,
		}
	}
	argo, _ := archive.Values["argo"].(map[string]any)
	if wif, ok := argo["releaseWIF"].(map[string]any); ok && len(wif) > 0 {
		targetArgo, _ := target.Values["argo"].(map[string]any)
		targetNamespace, _ := targetArgo["namespace"].(string)
		if targetNamespace == "" {
			targetNamespace = argoNamespace
		}
		current, _ := targetArgo["releaseWIF"].(map[string]any)
		switch {
		case wif["namespace"] != targetNamespace:
			notes = append(notes, fmt.Sprintf("argo.releaseWIF is recorded for namespace %v but the target runs pipelines in %s; it is not reapplied", wif["namespace"], targetNamespace))
		case canonicalJSON(current) != canonicalJSON(wif):
			overlay["argo"] = map[string]any{"releaseWIF": wif}
		}
	}
	if tunnel, ok := archive.Values["watchTunnel"].(map[string]any); ok && tunnel["enabled"] == true {
		notes = append(notes, "watchTunnel was enabled on the source; it pins the old server CA and needs the tunnel adoption flow, so it is not reapplied (see docs/watch-tunnel.md)")
	}
	if tls, ok := archive.Values["tls"].(map[string]any); ok {
		var names []string
		for _, key := range []string{"extraDNSNames", "extraIPs"} {
			if list, ok := tls[key].([]any); ok {
				for _, item := range list {
					names = append(names, fmt.Sprint(item))
				}
			}
		}
		if len(names) > 0 {
			notes = append(notes, "the source certificate also covered "+strings.Join(names, ", ")+"; pass the same --tls-extra-dns-name/--tls-extra-ip on your next oberth install")
		}
	}
	return overlay, notes
}

func printRestorePlan(w io.Writer, cfg RestoreConfig, deps backupDeps, archive restoreArchive, target restoreTarget) {
	m := archive.Manifest
	_, _ = fmt.Fprintf(w, "\nRestore plan for context %s (namespace %s, OpenBao %s, Argo %s):\n", deps.ContextName, cfg.Namespace, cfg.OpenBaoNamespace, cfg.ArgoNamespace)
	_, _ = fmt.Fprintf(w, "  Archive    %s\n             sha256 %s, created %s from context %s (%s)\n",
		cfg.Input, archive.CipherSHA, m.CreatedAt, displayOr(m.Source.Context, "?"), displayOr(m.Source.ServerVersion, "unknown server version"))
	// Same cluster means a rollback: every run created after the backup
	// disappears and its workflows are deleted below (review finding).
	switch {
	case m.Source.APIServer != "" && m.Source.APIServer == deps.apiServer:
		_, _ = fmt.Fprintf(w, "  Cluster    SAME cluster the backup was taken from (%s): this rolls Oberth back to %s; runs created since are dropped\n",
			deps.apiServer, m.CreatedAt)
	case m.Source.APIServer != "" && deps.apiServer != "":
		_, _ = fmt.Fprintf(w, "  Cluster    different cluster: backup from %s, restoring onto %s\n", m.Source.APIServer, deps.apiServer)
	}

	plan := target.OpenBao
	if plan.Known {
		_, _ = fmt.Fprintf(w, "  OpenBao    %d secrets (%d new, %d replacing current values, %d unchanged)\n",
			len(archive.KV.Secrets), plan.SecretsNew, plan.SecretsReplaced, plan.SecretsUnchanged)
		_, _ = fmt.Fprintf(w, "             %d policies (%d new, %d replaced, %d unchanged), %d roles (%d new, %d replaced, %d unchanged)\n",
			len(archive.Policies), len(plan.PoliciesNew), len(plan.PoliciesReplaced), plan.PoliciesUnchanged,
			len(archive.Roles.Roles), len(plan.RolesNew), len(plan.RolesReplaced), plan.RolesUnchanged)
		if len(plan.RolesReplaced) > 0 {
			_, _ = fmt.Fprintf(w, "             replaced roles: %s\n", strings.Join(plan.RolesReplaced, ", "))
		}
	} else {
		_, _ = fmt.Fprintf(w, "  OpenBao    write %d secrets, %d policies, %d roles (no root token: target not compared)\n",
			len(archive.KV.Secrets), len(archive.Policies), len(archive.Roles.Roles))
	}
	_, _ = fmt.Fprintln(w, "             each verified by read-back; Transit key, PKI CA and auth config are not restored")
	_, _ = fmt.Fprintf(w, "  Server     scale deploy/oberth %d -> 0, restore, then back to %d\n", target.Server.Replicas, max(target.Server.Replicas, 1))
	_, _ = fmt.Fprintf(w, "  Database   replace %s on PVC %s; current files kept in /data/pre-restore-%s/\n", serverDatabasePath, target.Server.ClaimName, target.Timestamp)
	_, _ = fmt.Fprintf(w, "             schema %d, audit head %d:%s\n", archive.Report.Schema, archive.Report.AuditHead.ID, m.Database.AuditHeadSHA256)
	if target.Pod == "" {
		_, _ = fmt.Fprintln(w, "             (no server pod is running: the target schema is checked in the restore pod before the swap)")
	}
	if archive.Grants.Present {
		_, _ = fmt.Fprintf(w, "  Grants     replace ConfigMap %s/%s (%d grants)\n", cfg.Namespace, secretAccessConfigMapName, countGrants(archive.Grants))
	} else {
		_, _ = fmt.Fprintf(w, "  Grants     the source had no %s ConfigMap; the target's is left as it is\n", secretAccessConfigMapName)
	}
	if len(target.Overlay) == 0 {
		_, _ = fmt.Fprintln(w, "  Helm       compatibility values already match; release oberth is not changed")
	} else {
		keys := make([]string, 0, len(target.Overlay))
		for key := range target.Overlay {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		_, _ = fmt.Fprintf(w, "  Helm       set %s on release oberth (--reuse-values)\n", strings.Join(keys, ", "))
		if _, ok := target.Overlay["compatibility"]; ok {
			have, want := target.Server.Compatibility, m.Compatibility
			_, _ = fmt.Fprintf(w, "             schemaIdentity %s -> %s, auditDomain %s -> %s, witnessKeyInfo %s -> %s\n",
				have.SchemaIdentity, want.SchemaIdentity, have.AuditDomain, want.AuditDomain, have.WitnessKeyInfo, want.WitnessKeyInfo)
		}
	}
	if len(target.Orphans) > 0 {
		names := make([]string, 0, len(target.Orphans))
		for _, wf := range target.Orphans {
			names = append(names, wf.Name)
		}
		_, _ = fmt.Fprintf(w, "  Workflows  delete %d whose runs the restored database does not know: %s\n", len(names), strings.Join(names, ", "))
	} else {
		_, _ = fmt.Fprintln(w, "  Workflows  none orphaned")
	}
	for _, note := range target.Notes {
		_, _ = fmt.Fprintf(w, "  Note       %s\n", note)
	}
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
	_, _ = fmt.Fprintf(deps.Output, "\nType the target context name (%s) to proceed: ", deps.ContextName)
	line, err := bufio.NewReader(deps.Input).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return errors.New("restore cancelled")
	}
	if strings.TrimSpace(line) != deps.ContextName {
		return errors.New("restore cancelled: the context name did not match")
	}
	return nil
}

// applyRestore performs the confirmed plan. Once the server has been scaled
// to zero, every failure says how to get back to a running server.
func applyRestore(ctx context.Context, cfg RestoreConfig, deps backupDeps, archive restoreArchive, target restoreTarget, bao openBaoExec, work string) error {
	w := deps.Output
	_, _ = fmt.Fprintln(w, "\nRestoring OpenBao...")
	written, err := restoreOpenBao(ctx, bao, deps.rootToken, archive.KV, archive.Policies, archive.Roles)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "  ✓ %d secrets (%d written, %d already current), %d policies, %d roles verified by read-back\n",
		len(archive.KV.Secrets), written, len(archive.KV.Secrets)-written, len(archive.Policies), len(archive.Roles.Roles))

	// Resolve the chart before stopping anything, so a missing chart cannot
	// leave the server scaled down.
	var helmArgs []string
	if len(target.Overlay) > 0 {
		var err error
		if helmArgs, err = restoreHelmArgs(ctx, cfg, deps, target, work); err != nil {
			return err
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

	_, _ = fmt.Fprintln(w, "Swapping the database on the volume...")
	if err := swapDatabase(ctx, cfg, deps, archive, target); err != nil {
		swapped = errors.Is(err, errRestoredDatabaseInPlace)
		return stopped(err)
	}
	swapped = true
	_, _ = fmt.Fprintf(w, "  ✓ restored database in place; the previous files are in /data/pre-restore-%s/ on PVC %s\n", target.Timestamp, target.Server.ClaimName)

	if archive.Grants.Present {
		if err := writeSecretAccess(ctx, deps.Deps, cfg.Namespace, archive.Grants); err != nil {
			return stopped(err)
		}
		_, _ = fmt.Fprintf(w, "  ✓ ConfigMap %s restored (%d grants)\n", secretAccessConfigMapName, countGrants(archive.Grants))
	}

	// List again now that no server can submit new workflows.
	if workflows, err := listRunWorkflows(ctx, deps, cfg.ArgoNamespace); err == nil {
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
		if _, err := deps.RunHelm(ctx, helmArgs); err != nil {
			return stopped(fmt.Errorf("set compatibility values on release oberth: %w", err))
		}
		_, _ = fmt.Fprintln(w, "  ✓ release oberth updated with the restored compatibility values")
	}

	_, _ = fmt.Fprintln(w, "Starting the server...")
	if err := scaleServer(ctx, deps.Deps, cfg.Namespace, replicas); err != nil {
		return stopped(err)
	}
	if err := waitForPodReady(ctx, deps.Deps, cfg.Namespace, target.Server.Selector, cfg.Timeout); err != nil {
		_, _ = fmt.Fprintf(w, "  ⚠ the server is not ready yet (%v); check: kubectl -n %s logs deploy/oberth\n", err, cfg.Namespace)
	} else {
		_, _ = fmt.Fprintln(w, "  ✓ server ready on the restored database")
	}
	printRestoreNextSteps(w, cfg, archive)
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

func scaleServer(ctx context.Context, deps Deps, namespace string, replicas int32) error {
	patch := fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas)
	if _, err := deps.KubeClient.AppsV1().Deployments(namespace).Patch(ctx, "oberth", types.MergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
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
	if out, err := inPod(archive.Database.Main, stageUploadScript, "oberth.sqlite"); err != nil {
		return fmt.Errorf("upload database: %w%s", err, commandOutputSuffix(out))
	}
	walSHA := "-"
	if archive.Database.WAL != nil {
		walSHA = archive.Database.WALSHA256
		if out, err := inPod(archive.Database.WAL, stageUploadScript, "oberth.sqlite-wal"); err != nil {
			return fmt.Errorf("upload database WAL: %w%s", err, commandOutputSuffix(out))
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

func printRestoreNextSteps(w io.Writer, cfg RestoreConfig, archive restoreArchive) {
	_, _ = fmt.Fprintln(w, "\nRestore complete. Next steps:")
	_, _ = fmt.Fprintln(w, "  1. Reconcile per-repo identities and policies from the restored grants (keep your usual install flags):")
	_, _ = fmt.Fprintln(w, "         read -rs BAO_TOKEN && export BAO_TOKEN")
	_, _ = fmt.Fprintln(w, "         oberth install --install-secretstore --upgrade")
	_, _ = fmt.Fprintln(w, "     It also refreshes this machine's pinned server CA.")
	_, _ = fmt.Fprintln(w, "  2. The server CA is new: every other CLI/MCP client must re-pin it (rerun oberth install there, or copy the new ca.crt).")
	_, _ = fmt.Fprintf(w, "  3. Uplinks are restored with the database; re-register any workstation whose key changed:\n"+
		"         kubectl exec -i -n %s deploy/oberth -- oberth uplink add - you@host < ~/.ssh/id_ed25519.pub\n", cfg.Namespace)
	_, _ = fmt.Fprintln(w, "  4. Trusted-plan envelopes sealed under the old Transit key cannot be decrypted; re-run the affected plans.")
	_, _ = fmt.Fprintln(w, "  5. Git caches on the volume were not replaced: a re-push of a branch the cache already holds is \"up to date\";")
	_, _ = fmt.Fprintln(w, "     push a new commit to trigger CI on the restored state.")
	if _, ok := archive.Values["watchTunnel"]; ok {
		_, _ = fmt.Fprintln(w, "  6. Re-establish the watch tunnel if you use it (docs/watch-tunnel.md).")
	}
}

// errRestoredDatabaseInPlace marks a swap failure that happened after the
// restored database was already in place.
var errRestoredDatabaseInPlace = errors.New("the restored database is in place but its final check failed")
