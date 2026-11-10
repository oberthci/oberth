package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/releaseverify"
	"github.com/oberthci/oberth/pkg/argoworkflow"

	"golang.org/x/mod/semver"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// canonicalGARPrefix is the canonical Google Artifact Registry prefix for
// Oberth release images. Published chart refs must resolve to this registry.
const canonicalGARPrefix = "europe-west4-docker.pkg.dev/skipopsmain/oberth/"

// DefaultUpgradeWaitTimeout bounds queue observation independently of rollout.
const DefaultUpgradeWaitTimeout = 2 * time.Hour

// UpgradeConfig holds options for the upgrade command.
type UpgradeConfig struct {
	AllowRetiredExecution bool          // explicit acceptance of retired workload/resource risk
	OwnershipHandoff      string        // operator-verified connector/site handoff receipt
	WaitTimeout           time.Duration // independent queue-observation limit
	WaitFor               string        // all or critical; a nonempty selection enables waiting
	WaitIdle              bool          // optional advisory queue drain; ordinary upgrades adopt in-flight work
	Namespace             string
	DryRun                bool
	Yes                   bool   // --yes: proceed without confirmation for non-local targets
	ChartOverride         string // --chart: override the chart reference
	BinaryVersion         string
	Timeout               time.Duration
	UserValues            []string // --values/-f: repeatable values file paths
	UserSet               []string // --set: repeatable key=value pairs
	ArgoControllerProfile string   // explicit supported controller transition; empty preserves existing profile
	BinarySchemaVersion   int      // set by the caller to store.LatestMigrationVersion()
	TLSExtraDNSNames      []string
	TLSExtraIPs           []string
	TLSRemoveDNSNames     []string
	TLSRemoveIPs          []string
	OpenBaoNamespace      string
	// ReleaseDir reads the signed release objects from a directory instead
	// of fetching them (air-gapped hosts). ReleaseOrigin and ChartRepository
	// override the publication origins for a mirror; verification is the same.
	ReleaseDir      string
	ReleaseOrigin   string
	ChartRepository string
	// NoBackup skips the pre-upgrade backup (explicit --no-backup); Backup
	// takes it. Without either the upgrade refuses before any write.
	NoBackup bool
	Backup   func(context.Context) (UpgradeBackup, error)
	// RecordDir is where the rollback record is written (default: next to
	// the backup, else the current directory).
	RecordDir string
	// AllowDowngrade accepts a target older than the installed release, for
	// a deliberate rollback within one database schema.
	AllowDowngrade bool
	serverPKI      bool
	// verifiedChart is set once the chart is the verified local file of a
	// signed release; Helm then gets no --version.
	verifiedChart bool
	storedValues  string // internal schema-validated legacy migration snapshot
}

// ErrPinnedKeyOverride is returned when a user --set or --values file
// attempts to override a value the upgrade path pins for safety.
type ErrPinnedKeyOverride struct {
	Key string
}

func (e *ErrPinnedKeyOverride) Error() string {
	if isPinnedUpgradeKey(e.Key, upgradePKIPinnedKeys) {
		return fmt.Sprintf("refused: %q is pinned because server PKI is enabled on the live release; use --tls-extra-dns-name/--tls-extra-ip or --tls-remove-dns-name/--tls-remove-ip to change TLS names", e.Key)
	}
	return fmt.Sprintf("refused: %q is pinned by the upgrade and cannot be overridden via --set or --values", e.Key)
}

// ErrSchemaValidation is returned when a user value fails chart schema
// validation before the Helm call.
type ErrSchemaValidation struct {
	Detail string
}

func (e *ErrSchemaValidation) Error() string {
	return fmt.Sprintf("chart schema validation failed: %s", e.Detail)
}

// upgradePinnedKeys are the helm values protected by the upgrade path. The
// enablement of an existing private module proxy is preserved from stored values;
// callers cannot disable that existing routing boundary as an upgrade override.
var upgradePinnedKeys = map[string]bool{
	"image.ref":                    true,
	"argo.goProxy.enabled":         true,
	"argo.goProxy.port":            true,
	"compatibility.schemaIdentity": true,
	"compatibility.auditDomain":    true,
	"compatibility.witnessKeyInfo": true,
}

// Only a release already using server PKI needs TLS reconciliation through
// the dedicated flags. Other releases retain ordinary Helm TLS overrides.
var upgradePKIPinnedKeys = map[string]bool{
	"tls.extraDNSNames": true,
	"tls.extraIPs":      true,
	"tls.pki":           true,
}

// UpgradeResult describes the outcome of an upgrade.
type UpgradeResult struct {
	PreviousVersion string
	TargetVersion   string
	AlreadyUpToDate bool
	Upgraded        bool
	Migrated        string // legacy key/value stripped from stored configuration
	RecordPath      string // rollback record written before the first change
}

// ValidateUpgrade checks UpgradeConfig for invalid flag combinations and
// applies defaults.
func (cfg *UpgradeConfig) ValidateUpgrade() error {
	tls := Config{Namespace: cfg.Namespace, TLSExtraDNSNames: cfg.TLSExtraDNSNames, TLSExtraIPs: cfg.TLSExtraIPs,
		TLSRemoveDNSNames: cfg.TLSRemoveDNSNames, TLSRemoveIPs: cfg.TLSRemoveIPs}
	if err := validateServerTLSFlags(&tls); err != nil {
		return err
	}
	cfg.TLSExtraIPs, cfg.TLSRemoveIPs = tls.TLSExtraIPs, tls.TLSRemoveIPs
	if cfg.ArgoControllerProfile != "" && cfg.ArgoControllerProfile != "nonroot-static-v1" {
		return errors.New("unsupported Argo controller profile")
	}
	if cfg.WaitFor != "" {
		if cfg.WaitFor != "all" && cfg.WaitFor != "critical" {
			return errors.New("--wait-for must be all or critical")
		}
		cfg.WaitIdle = true
	}
	if cfg.WaitTimeout < 0 {
		return errors.New("--wait-timeout must be positive")
	}
	if cfg.WaitTimeout == 0 {
		cfg.WaitTimeout = DefaultUpgradeWaitTimeout
	}
	if cfg.Namespace == "" {
		cfg.Namespace = DefaultNamespace
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.BinaryVersion == "" || cfg.BinaryVersion == "dev" {
		return errors.New("cannot upgrade: this binary has no release version (dev build); use --chart with an explicit chart reference")
	}
	for _, kv := range cfg.UserSet {
		if !strings.Contains(kv, "=") {
			return fmt.Errorf("invalid --set value %q: expected key=value format", kv)
		}
	}
	for _, path := range cfg.UserValues {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("values file %s: %w", path, err)
		}
	}
	return nil
}

// checkPinnedKeyConflicts checks that user --set and --values do not attempt
// to override installer-pinned keys.
func checkPinnedKeyConflicts(userSet []string, userValues []string) error {
	return checkUpgradePinnedKeyConflicts(userSet, userValues, upgradePinnedKeys)
}

func checkUpgradePKIKeyConflicts(cfg UpgradeConfig, values map[string]any) error {
	// These are the selected deployed revision's stored values, before
	// applying any user overrides (including attempts to disable PKI).
	tls, _ := values["tls"].(map[string]any)
	pki, _ := tls["pki"].(map[string]any)
	if pki["enabled"] != true {
		return nil
	}
	return checkUpgradePinnedKeyConflicts(cfg.UserSet, cfg.UserValues, upgradePKIPinnedKeys)
}

func checkUpgradePinnedKeyConflicts(userSet []string, userValues []string, pinnedKeys map[string]bool) error {
	for _, kv := range userSet {
		key, err := upgradeSetKey(kv)
		if err != nil {
			return err
		}
		if isPinnedUpgradeKey(key, pinnedKeys) {
			return &ErrPinnedKeyOverride{Key: key}
		}
	}
	for _, path := range userValues {
		data, err := os.ReadFile(path) //nolint:gosec // G304: path is user-supplied --values flag, intentionally read
		if err != nil {
			return fmt.Errorf("read values file %s: %w", path, err)
		}
		if err := checkUpgradeValuesPinnedConflicts(data, pinnedKeys); err != nil {
			return err
		}
	}
	return nil
}

// isPinnedUpgradeKey reports whether key (a dotted Helm value path) conflicts
// with an installer-pinned key: either an exact match or a parent path that
// would wipe a pinned subtree.
func isPinnedUpgradeKey(key string, pinnedKeys map[string]bool) bool {
	if pinnedKeys[key] {
		return true
	}
	for pinned := range pinnedKeys {
		if strings.HasPrefix(pinned, key+".") || strings.HasPrefix(key, pinned+".") {
			return true
		}
	}
	return false
}

func checkUpgradeValuesPinnedConflicts(data []byte, pinnedKeys map[string]bool) error {
	var values map[string]any
	if err := yaml.Unmarshal(data, &values); err != nil {
		return fmt.Errorf("parse values YAML: %w", err)
	}
	leafPaths := make(map[string]bool)
	collectLeafKeyPaths(values, "", leafPaths)
	for path := range leafPaths {
		if isPinnedUpgradeKey(path, pinnedKeys) {
			return &ErrPinnedKeyOverride{Key: path}
		}
	}
	return nil
}

// collectLeafKeyPaths walks a nested map and collects dotted key paths for
// all non-map leaf values.
func collectLeafKeyPaths(m map[string]any, prefix string, out map[string]bool) {
	for key, value := range m {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if sub, ok := value.(map[string]any); ok {
			collectLeafKeyPaths(sub, path, out)
		} else {
			out[path] = true
		}
	}
}

// validateUserSetAgainstValues checks that each --set key path exists in the
// chart's default values hierarchy. This catches typos before helm runs.
func validateUserSetAgainstValues(valuesYAML []byte, userSet []string) error {
	if len(userSet) == 0 {
		return nil
	}
	var values map[string]any
	if err := yaml.Unmarshal(valuesYAML, &values); err != nil {
		return fmt.Errorf("parse chart values for schema validation: %w", err)
	}
	for _, kv := range userSet {
		key, err := upgradeSetKey(kv)
		if err != nil {
			return err
		}
		if !argoworkflow.RetiredValue(key) && !keyPathExists(values, key) {
			return &ErrSchemaValidation{Detail: fmt.Sprintf("key %q does not exist in the chart values; check for typos", key)}
		}
	}
	return nil
}

// keyPathExists reports whether a dotted key path resolves to any value in
// the nested map (leaf or intermediate).
func keyPathExists(values map[string]any, key string) bool {
	parts := strings.Split(key, ".")
	current := values
	for i, part := range parts {
		value, ok := current[part]
		if !ok {
			return false
		}
		if i == len(parts)-1 {
			return true
		}
		sub, ok := value.(map[string]any)
		if !ok {
			return false
		}
		current = sub
	}
	return true
}

// RunUpgrade executes the upgrade flow: detect current version, compare with
// the binary's version, upgrade the Helm release, wait for rollout, and
// verify the new version.
func RunUpgrade(ctx context.Context, cfg UpgradeConfig, deps Deps) (UpgradeResult, error) {
	if err := captureBaoEnvironment(&deps); err != nil {
		return UpgradeResult{}, err
	}
	if err := cfg.ValidateUpgrade(); err != nil {
		return UpgradeResult{}, err
	}

	w := deps.Output
	if w == nil {
		w = io.Discard
	}
	color := isColor(deps)

	targetVersion := cfg.BinaryVersion
	_, _ = fmt.Fprintf(w, "oberth upgrade %s\n\n", displayVersion(targetVersion))

	// Print the target cluster context and server before any mutation.
	server := ""
	if deps.RestConfig != nil {
		server = deps.RestConfig.Host
	}
	_, _ = fmt.Fprintf(w, "Target: %s (%s)\n", deps.ContextName, server)

	// Non-local safety guard: match install's pattern (installer.go:468-475).
	// An upgrade against a remote cluster without explicit acknowledgement
	// risks applying to the wrong environment.
	if !IsLocalServer(server) && !cfg.Yes {
		return UpgradeResult{}, fmt.Errorf("target %q does not appear to be a local cluster (server: %s); "+
			"use --yes to proceed or switch to a local context", deps.ContextName, server)
	}
	if !IsLocalServer(server) && cfg.Yes {
		_, _ = fmt.Fprintf(w, "WARNING: %q does not appear to be a local cluster; proceeding because --yes was set\n", deps.ContextName)
	}

	// Detect current version from the Helm release.
	release, exists, err := findHelmRelease(ctx, deps, "oberth", cfg.Namespace)
	if err != nil {
		return UpgradeResult{}, err
	}
	if !exists {
		return UpgradeResult{}, fmt.Errorf("no Oberth Helm release found in namespace %s; run oberth install first", cfg.Namespace)
	}
	currentVersion := chartVersionFromRelease(release.Chart, "oberth")

	result := UpgradeResult{
		PreviousVersion: currentVersion,
		TargetVersion:   targetVersion,
	}

	phase(w, "Current version", displayVersion(currentVersion), color)

	// Compare versions. Refuse to proceed when the installed version cannot
	// be parsed (unknown state) or is strictly newer than the CLI (silent
	// downgrade).
	currentCanonical := canonicalChartVersion(currentVersion)
	targetCanonical := canonicalChartVersion(targetVersion)
	if targetCanonical == "" {
		return result, fmt.Errorf("target version %q is not a valid semantic version", targetVersion)
	}
	if currentCanonical == "" {
		if currentVersion == "" {
			return result, errors.New("no version could be determined from the installed Helm release; " +
				"refusing to upgrade an unknown release — use helm upgrade directly or reinstall with oberth install")
		}
		return result, fmt.Errorf("installed version %q could not be parsed; refusing to upgrade an unknown release — "+
			"use helm upgrade directly or reinstall with oberth install", currentVersion)
	}
	cmp := semver.Compare(currentCanonical, targetCanonical)
	status := strings.ToLower(strings.TrimSpace(release.Status))
	revision := helmRevisionLabel(release.Revision)
	retryFailed := false
	if cmp == 0 {
		switch {
		case status == "deployed" && upgradeHasTLSFlags(cfg):
			// Explicit TLS changes require the same-version reconciliation path.
		case status == "deployed" && cfg.ArgoControllerProfile != "":
		// Explicit supported profile reconciliation is not a same-version no-op.
		case status == "deployed" && (len(cfg.UserValues) > 0 || len(cfg.UserSet) > 0):
			// Explicit --values/--set are a request to change the release, so
			// re-apply them through the same pinned upgrade path (#055).
			_, _ = fmt.Fprintf(w, "\nAlready running %s; re-applying the given --values/--set\n", displayVersion(targetVersion))
		case status == "deployed":
			result.AlreadyUpToDate = true
			_, _ = fmt.Fprintf(w, "\nAlready running %s; no upgrade performed\n", displayVersion(targetVersion))
			return result, nil
		case status == "failed":
			// A failed revision can carry the target chart version while a
			// healthy Deployment still runs. Retry it through the same pinned
			// image, Helm upgrade, rollout, and version-verification path.
			retryFailed = true
			_, _ = fmt.Fprintf(w, "\nHelm release %q%s is FAILED at %s; same-version forward repair required.\n", release.Name, revision, displayVersion(currentVersion))
		case strings.HasPrefix(status, "pending"):
			return result, fmt.Errorf("helm release %q%s is %q at version %s; a Helm operation may still be in progress — inspect helm status %s -n %s before retrying", release.Name, revision, release.Status, displayVersion(currentVersion), release.Name, cfg.Namespace)
		default:
			return result, fmt.Errorf("helm release %q%s has unknown state %q at version %s; refusing a same-version no-op or retry — inspect helm status %s -n %s", release.Name, revision, release.Status, displayVersion(currentVersion), release.Name, cfg.Namespace)
		}
	}
	if cmp > 0 && !cfg.AllowDowngrade {
		return result, fmt.Errorf("installed version %s is newer than CLI version %s — upgrade the CLI first "+
			"(a deliberate rollback is oberth upgrade --rollback <record>, or --allow-downgrade within one database schema)",
			displayVersion(currentVersion), displayVersion(targetVersion))
	}
	if cmp > 0 {
		_, _ = fmt.Fprintf(w, "WARNING: --allow-downgrade: moving the server from %s back to %s\n", displayVersion(currentVersion), displayVersion(targetVersion))
	}

	// Validate user overrides early (no network needed).
	if err := checkPinnedKeyConflicts(cfg.UserSet, cfg.UserValues); err != nil {
		return result, err
	}

	if err := checkUpgradeOwnership(ctx, cfg, deps, currentVersion); err != nil {
		return result, err
	}
	if semver.Compare(targetCanonical, "v0.17.83-0") >= 0 {
		if err := checkUpgradeLegacyResources(ctx, cfg, deps); err != nil {
			return result, err
		}
		if err := checkUpgradeLegacyRepositories(ctx, cfg, deps, currentVersion); err != nil {
			return result, err
		}
	}

	profile, cleanupProfile, err := prepareUpgradeControllerProfile(ctx, cfg, deps)
	if err != nil {
		return result, err
	}
	defer cleanupProfile()

	// Announce a known or possible migration before any mutation. The
	// upgrade takes the backup itself (takeUpgradeBackup) unless --no-backup.
	liveSchema := 0
	if cfg.BinarySchemaVersion > 0 {
		var schemaErr error
		liveSchema, schemaErr = queryRunningSchemaVersion(ctx, deps, cfg)
		if schemaErr != nil || liveSchema <= 0 {
			if cmp > 0 {
				return result, errors.New("--allow-downgrade needs the running database schema, which could not be read: " +
					"an older server cannot open a newer database; roll back with oberth upgrade --rollback <record> instead")
			}
			liveSchema = 0
			_, _ = fmt.Fprintln(w, "\nWARNING: running database schema is unknown; the target may run forward-only migrations.")
			warnUpgradeBackup(w, cfg)
		} else if liveSchema < cfg.BinarySchemaVersion {
			_, _ = fmt.Fprintf(w, "\nSchema migration v%d will run (forward-only; rolling back restores the pre-upgrade backup)\n", cfg.BinarySchemaVersion)
			warnUpgradeBackup(w, cfg)
		} else if liveSchema > cfg.BinarySchemaVersion {
			return result, fmt.Errorf("the server writes database schema %d, newer than this CLI's %d, which the target server cannot open; "+
				"upgrade the CLI first, or roll back with oberth upgrade --rollback <record>, which restores the pre-upgrade backup", liveSchema, cfg.BinarySchemaVersion)
		}
	}

	if cfg.DryRun {
		revision, err := deployedUpgradeRevision(ctx, cfg.Namespace, deps)
		if err != nil {
			return result, err
		}
		values, err := readUpgradeStoredValues(ctx, cfg.Namespace, revision, deps)
		if err != nil {
			return result, err
		}
		warnRetiredUpgradeValues(w, cfg, values)
		if err := checkUpgradePKIKeyConflicts(cfg, values); err != nil {
			return result, err
		}
		if _, err := prepareUpgradeServerPKI(ctx, &cfg, deps, values); err != nil {
			return result, err
		}
		result.Migrated, err = migrateUpgradeShadow(values)
		if err != nil {
			return result, err
		}
		if result.Migrated != "" {
			cfg.storedValues = "<cleaned-deployed-values.json>"
			_, _ = fmt.Fprintf(w, "Would strip stored legacy %s from deployed revision %s before applying the cleaned snapshot.\n", result.Migrated, revision)
		}
		if profile != nil {
			profile.describe(w, cfg.Timeout)
		}
		return printUpgradeDryRun(w, cfg, result, retryFailed)
	}

	if err := requireUpgradeHelmVersion(ctx, deps); err != nil {
		return result, err
	}

	// Resolve the chart: the verified chart of the signed release for this
	// CLI's version, or an explicit --chart (development; unverified).
	chart := cfg.ChartOverride
	var signed *verifiedUpgradeRelease
	if chart == "" {
		signed, err = verifyUpgradeRelease(ctx, cfg, deps)
		if err != nil {
			return result, err
		}
		defer signed.cleanup()
		signed.describe(w)
		chart = signed.chartPath
		cfg.verifiedChart = true
	} else {
		_, _ = fmt.Fprintf(w, "WARNING: --chart %s is not a signed release; its contents are not verified.\n", cfg.ChartOverride)
	}

	// Resolve the target chart's image ref so the upgrade pins the exact image.
	imageRef, valuesYAML, err := resolveChartImageRef(ctx, deps, chart, targetVersion, true)
	if err != nil {
		return result, fmt.Errorf("resolve chart image: %w", err)
	}
	if signed != nil {
		if err := signed.requireRecordImage(imageRef); err != nil {
			return result, err
		}
	}

	// Validate user --set keys against the chart values schema.
	if err := validateUserSetAgainstValues(valuesYAML, cfg.UserSet); err != nil {
		return result, err
	}

	prepared, err := prepareUpgradeValues(ctx, cfg, deps, chart, imageRef)
	if err != nil {
		return result, err
	}

	defer prepared.cleanup()
	cfg.storedValues = prepared.storedValues
	result.Migrated = prepared.migrated
	compatibility := prepared.compatibility
	pki := prepared.pki
	pki.configure(&cfg)

	previewArgs, previewFlags := upgradeHelmArgs(cfg, chart, imageRef, compatibility)
	previewArgs = append(previewArgs, previewFlags...)
	previewArgs = append(previewArgs, "--dry-run=server", "--no-hooks", "--output=json")
	previewRaw, previewErr := deps.RunHelm(ctx, previewArgs)
	if previewErr != nil {
		clear(previewRaw)
		return result, fmt.Errorf("preview upgrade before writes: %w", previewErr)
	}
	previewErr = printHelmValueChanges(ctx, deps, cfg.Namespace, previewRaw, prepared.revision)
	clear(previewRaw)
	if previewErr != nil {
		return result, previewErr
	}

	if cfg.WaitIdle {
		if err := waitForUpgradeIdle(ctx, cfg, deps); err != nil {
			return result, err
		}
	}

	// Recheck after preview/drain, immediately before the first live write.
	if err := checkUpgradeOwnership(ctx, cfg, deps, currentVersion); err != nil {
		return result, err
	}
	if semver.Compare(targetCanonical, "v0.17.83-0") >= 0 {
		if err := checkUpgradeLegacyResources(ctx, cfg, deps); err != nil {
			return result, err
		}
		if err := checkUpgradeLegacyRepositories(ctx, cfg, deps, currentVersion); err != nil {
			return result, err
		}
	}

	// The backup and the rollback record come last before the first write,
	// so they hold the state the upgrade starts from.
	backupPath, err := takeUpgradeBackup(ctx, cfg, w)
	if err != nil {
		return result, err
	}
	record := UpgradeRecord{
		Format: upgradeRecordFormat, CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Context: deps.ContextName, APIServer: server, ClusterUID: kubeSystemUID(ctx, deps), Namespace: cfg.Namespace,
		From: currentVersion, To: targetVersion, OberthRevision: helmRevisionNumber(release.Revision),
		SchemaBefore: liveSchema, SchemaAfter: cfg.BinarySchemaVersion, Backup: backupPath,
	}
	if profile != nil {
		record.ArgoNamespace, record.ArgoRevision = profile.namespace, profile.revisionNumber
	}
	if record.OberthRevision > 0 && (cfg.RecordDir != "" || backupPath != "") {
		recordPath, err := writeUpgradeRecord(cfg, record)
		if err != nil {
			return result, err
		}
		result.RecordPath = recordPath
		_, _ = fmt.Fprintf(w, "Rollback record: %s (roll back with: oberth upgrade --rollback %s)\n", recordPath, recordPath)
	}

	if profile != nil {
		if err := profile.apply(ctx, cfg, deps); err != nil {
			return result, fmt.Errorf("argo controller profile transition incomplete: %w", err)
		}
	}
	if pki != nil {
		_, replaced, err := ensureServerPKI(ctx, pki.config, pki.store, pki.token, map[string]baoMount{defaultServerPKIMount + "/": {Type: "pki"}})
		if err != nil {
			return result, fmt.Errorf("reconcile server PKI before upgrade: %w", err)
		}
		if replaced {
			_, _ = fmt.Fprintln(w, "Server CA replaced; clients pinning the previous CA must refresh their trust anchor.")
		}
	}

	// Build and execute the helm upgrade.
	phase(w, "Chart", "updating", color)
	args, flags := upgradeHelmArgs(cfg, chart, imageRef, compatibility)
	args = append(args, flags...)
	if _, err := deps.RunHelm(ctx, args); err != nil {
		printRecoveryGuidance(w, cfg.Namespace)
		return result, fmt.Errorf("helm upgrade: %w", err)
	}
	phase(w, "Oberth", "upgrading", color)

	// Wait for rollout to complete by watching the deployment status.
	if err := waitForUpgradeRollout(ctx, deps, cfg, imageRef); err != nil {
		printRecoveryGuidance(w, cfg.Namespace)
		return result, fmt.Errorf("rollout: %w", err)
	}
	phase(w, "Rollout", "✓ ready", color)

	// Verify the running version matches the target.
	runningVersion, err := verifyRunningVersion(ctx, deps, cfg, targetVersion)
	if err != nil {
		printRecoveryGuidance(w, cfg.Namespace)
		return result, fmt.Errorf("version verification failed after upgrade: %w", err)
	}
	phase(w, "Oberth", "✓ "+displayVersion(runningVersion), color)

	if profile != nil {
		if err := profile.verifyFinal(ctx, cfg, deps); err != nil {
			return result, fmt.Errorf("controller transition incomplete after Oberth upgrade: %w", err)
		}
	}
	result.Upgraded = true
	deps.Output = w
	details := map[string]any{"from": currentVersion, "to": targetVersion}
	if result.Migrated != "" {
		details["migrated"] = result.Migrated
	}
	recordServerAudit(ctx, deps, cfg.Namespace, "deploy/oberth", "upgrade.completed", details)
	_, _ = fmt.Fprintf(w, "\nReady — Oberth deployment in namespace %s (context %s)\n", cfg.Namespace, deps.ContextName)
	return result, nil
}

// warnUpgradeBackup says what protects a migrating upgrade: the backup it
// takes itself before the first write, or, with --no-backup, nothing.
func warnUpgradeBackup(w io.Writer, cfg UpgradeConfig) {
	if cfg.NoBackup {
		_, _ = fmt.Fprintf(w, "WARNING: --no-backup: NO BACKUP WILL BE TAKEN by this upgrade.\n"+
			"  Migrations are forward-only: without a backup of namespace %s, Helm rollback alone cannot restore the database.\n", cfg.Namespace)
		return
	}
	_, _ = fmt.Fprintf(w, "  A history-only backup of namespace %s is taken before the first change and verified;\n"+
		"  oberth upgrade --rollback restores it if you roll back. Keep its private key off this machine.\n", cfg.Namespace)
}

// helmRevisionLabel accepts Helm's quoted and numeric JSON revision forms.
// An unreadable revision never obscures the release status or blocks recovery.
func helmRevisionLabel(raw json.RawMessage) string {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		value = string(raw)
	}
	number, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	if err != nil || number == 0 {
		return ""
	}
	return fmt.Sprintf(" revision %d", number)
}

// resolveChartImageRef reads the chart's default image.ref value so the
// upgrade pins the exact image the chart was built against, rather than
// relying on an implicit default that stored configuration might override with a
// stale value. It also returns the raw values YAML for downstream schema
// validation of user-supplied --set keys.
func resolveChartImageRef(ctx context.Context, deps Deps, chart, targetVersion string, isLocalChart bool) (string, []byte, error) {
	args := []string{"show", "values", chart}
	if !isLocalChart && targetVersion != "" {
		args = append(args, "--version", helmChartVersion(targetVersion))
	}
	valuesYAML, err := deps.RunHelm(ctx, args)
	if err != nil {
		return "", nil, fmt.Errorf("read chart values: %w", err)
	}
	var values oberthChartValues
	if err := yaml.Unmarshal(valuesYAML, &values); err != nil {
		return "", nil, fmt.Errorf("parse chart values: %w", err)
	}
	ref := strings.TrimSpace(values.Image.Ref)
	if ref == "" {
		return "", nil, errors.New("chart does not define image.ref")
	}

	// Validate the resolved ref is digest-pinned: <registry>/<repo>@sha256:<64hex>.
	// A tag-only ref could be silently replaced by stored configuration or a
	// registry push, so the upgrade must pin an immutable digest.
	repo, digest, isDigest := splitImageDigestRef(ref)
	if !isDigest || !isSHA256Digest(digest) {
		return "", nil, fmt.Errorf("chart image.ref %q is not in digest-pinned form (<registry>/<repo>@sha256:<64hex>)", ref)
	}

	// Validate the registry prefix for published charts. The --chart dev-loop
	// override allows any registry (local builds, mirrors) but still requires
	// digest form above.
	if !isLocalChart && !strings.HasPrefix(repo, canonicalGARPrefix) {
		return "", nil, fmt.Errorf("chart image.ref %q does not start with the canonical GAR prefix %s", ref, canonicalGARPrefix)
	}

	return ref, valuesYAML, nil
}

// upgradeHelmArgs returns the command prefix and flags for upgrading Oberth.
// The schema preflight reuses only flags, independent of the prefix length.
//
// DELIBERATE: --atomic is NOT used. Oberth's database migrations are
// forward-only; an automatic rollback to a pre-migration schema would leave
// the database in an inconsistent state (crashloop trap). On failure the
// operator must fix forward with a new 'oberth upgrade'.
func upgradeHelmArgs(cfg UpgradeConfig, chart, imageRef string, compatibility backupCompatibility) (prefix, flags []string) {
	prefix = []string{"upgrade", "oberth", chart, "-n", cfg.Namespace}
	// Merge new defaults below stored configuration and user overrides. When
	// migrating shadow, reuse the validated snapshot so Helm cannot resurrect it.
	args := []string{"--reset-then-reuse-values"}
	if cfg.storedValues != "" {
		args = []string{"--reset-values", "--values", cfg.storedValues}
	}
	args = append(args,
		"--set-string", "image.ref="+imageRef,
		"--set-string", "compatibility.schemaIdentity="+compatibility.SchemaIdentity,
		"--set-string", "compatibility.auditDomain="+compatibility.AuditDomain,
		"--set-string", "compatibility.witnessKeyInfo="+compatibility.WitnessKeyInfo,
		// Keep the existing proxy enablement and mapping from reused values.
		"--set", "argo.goProxy.port=8444",
	)
	if cfg.ArgoControllerProfile != "" {
		// Clear any pinned profile name: the server follows the profile the
		// controller runs (#1517).
		args = append(args, "--set-string", "argo.controllerProfile="+cfg.ArgoControllerProfile,
			"--set-string", "argo.controllerProfileConfigMap=")
	}
	// User values files come after pins. Pinned-key conflicts have already
	// been refused by checkPinnedKeyConflicts, so these cannot override a pin.
	for _, f := range cfg.UserValues {
		args = append(args, "--values", f)
	}
	// User --set values come after pins and values files.
	for _, kv := range cfg.UserSet {
		args = append(args, "--set", kv)
	}
	if cfg.serverPKI {
		dns, _ := json.Marshal(append([]string{}, cfg.TLSExtraDNSNames...))
		ips, _ := json.Marshal(append([]string{}, cfg.TLSExtraIPs...))
		args = append(args, "--set-json", "tls.extraDNSNames="+string(dns), "--set-json", "tls.extraIPs="+string(ips))
	}
	// Only pin --version for repository charts; a local path (an explicit
	// --chart or the verified release chart) IS the version.
	if cfg.ChartOverride == "" && !cfg.verifiedChart && cfg.BinaryVersion != "" {
		args = append(args, "--version", helmChartVersion(cfg.BinaryVersion))
	}
	// Pass the user's timeout to helm so it respects the same deadline.
	if cfg.Timeout > 0 {
		args = append(args, "--timeout", cfg.Timeout.String())
	}
	args = append(args, "--wait")
	return prefix, args
}

// waitForUpgradeRollout watches the deployment until all replicas are updated
// and available with the target image, or the timeout expires.
func waitForUpgradeRollout(ctx context.Context, deps Deps, cfg UpgradeConfig, targetImageRef string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	for {
		deployment, err := getOberthDeployment(deadline, deps, cfg.Namespace)
		if err != nil {
			return err
		}
		if deploymentRolledOut(deployment, targetImageRef) {
			return nil
		}
		select {
		case <-deadline.Done():
			if err := ctx.Err(); err != nil {
				return err
			}
			return fmt.Errorf("timeout (%s) waiting for Oberth deployment rollout in namespace %s: %w", cfg.Timeout, cfg.Namespace, deadline.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

// deploymentRolledOut reports whether the deployment has finished rolling out
// the target image: generation observed, all replicas updated and available,
// none unavailable, and the pod template specifies the expected image.
func deploymentRolledOut(deployment *appsv1.Deployment, targetImageRef string) bool {
	if deployment.Spec.Replicas == nil {
		return false
	}
	if deployment.Status.ObservedGeneration < deployment.Generation {
		return false
	}
	if deployment.Status.UpdatedReplicas != *deployment.Spec.Replicas {
		return false
	}
	if deployment.Status.AvailableReplicas != *deployment.Spec.Replicas {
		return false
	}
	if deployment.Status.UnavailableReplicas != 0 {
		return false
	}
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name == "oberth" && container.Image == targetImageRef {
			return true
		}
	}
	return false
}

// getOberthDeployment fetches the Oberth deployment by label selector.
func getOberthDeployment(ctx context.Context, deps Deps, namespace string) (*appsv1.Deployment, error) {
	if deps.KubeClient == nil {
		return nil, errors.New("no Kubernetes client available")
	}
	deployments, err := deps.KubeClient.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/instance=oberth",
	})
	if err != nil {
		return nil, fmt.Errorf("list Oberth deployments: %w", err)
	}
	if len(deployments.Items) == 0 {
		return nil, fmt.Errorf("no Oberth deployment found in namespace %s", namespace)
	}
	return &deployments.Items[0], nil
}

// queryRunningSchemaVersion execs into the Oberth pod and runs
// `oberth version --schema` to obtain the integer schema version.
// Returns 0 on any error; the caller warns loudly that migration/backup
// safety is unknown rather than silently omitting backup guidance.
//
// Old binaries that do not support --schema return a usage error;
// the caller falls through to the unknown-schema warning path.
func queryRunningSchemaVersion(ctx context.Context, deps Deps, cfg UpgradeConfig) (int, error) {
	run := deps.RunCommand
	if run == nil {
		run = DefaultRunCommand
	}
	args := []string{"exec", "-c", "oberth", "-n", cfg.Namespace, "deploy/oberth", "--", "/usr/local/bin/oberth", "version", "--schema"}
	if deps.ContextName != "" {
		args = append(args[:1], append([]string{"--context", deps.ContextName}, args[1:]...)...)
	}
	out, err := run(ctx, nil, "kubectl", args...)
	if err != nil {
		return 0, nil // old binary without --schema support, or exec failure
	}
	n, parseErr := strconv.Atoi(strings.TrimSpace(string(out)))
	if parseErr != nil {
		return 0, nil
	}
	return n, nil
}

// printRecoveryGuidance writes a short recovery block to w, covering release
// state, pod logs, and the forward-only rollback warning. Called on all three
// post-mutation failure paths (helm upgrade, rollout wait, version verify).
func printRecoveryGuidance(w io.Writer, namespace string) {
	_, _ = fmt.Fprintf(w, "\nRecovery:\n")
	_, _ = fmt.Fprintf(w, "  Release state:  helm status oberth -n %s\n", namespace)
	_, _ = fmt.Fprintf(w, "  Pod logs:       kubectl logs -n %s deploy/oberth\n", namespace)
	_, _ = fmt.Fprintf(w, "\nDo not run a bare 'helm rollback': database migrations are forward-only.\n"+
		"Fix the issue and run 'oberth upgrade' again, or roll back with 'oberth upgrade --rollback <record>'\n"+
		"(the rollback record printed above), which also restores the pre-upgrade backup when a migration ran.\n")
}

// verifyRunningVersion execs into the Oberth pod, runs `oberth version`,
// parses the reported version, and compares it against the expected target.
// Returns the running version on match, or an error on mismatch or parse
// failure.
//
// The exec subresource can lag right after rollout, so transient exec errors
// are retried up to verifyMaxAttempts times with pollInterval delays (the same
// pattern as upstreamConfigured in onboard.go). Version-mismatch and
// unparseable-output results are definitive and are NOT retried.
//
// verifyMaxAttempts is the number of exec attempts before giving up.
const verifyMaxAttempts = 3

func verifyRunningVersion(ctx context.Context, deps Deps, cfg UpgradeConfig, targetVersion string) (string, error) {
	run := deps.RunCommand
	if run == nil {
		run = DefaultRunCommand
	}
	args := []string{"exec", "-c", "oberth", "-n", cfg.Namespace, "deploy/oberth", "--", "/usr/local/bin/oberth", "version"}
	if deps.ContextName != "" {
		args = append(args[:1], append([]string{"--context", deps.ContextName}, args[1:]...)...)
	}

	var lastErr error
	for attempt := 0; attempt < verifyMaxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(pollInterval(deps)):
			}
		}
		out, err := run(ctx, nil, "kubectl", args...)
		if err != nil {
			lastErr = fmt.Errorf("kubectl exec oberth version: %w", err)
			continue // transient exec error — retry
		}
		// Parse "oberth v0.12.35 commit=... date=..."
		// Filter out kubectl stderr noise (e.g. "Defaulted container...")
		var runningVersion string
		for _, field := range strings.Fields(strings.TrimSpace(string(out))) {
			if strings.HasPrefix(field, "v") && semver.IsValid(field) {
				runningVersion = field
				break
			}
		}
		if runningVersion == "" {
			// Unparseable output is a definitive failure — do not retry.
			return "", fmt.Errorf("could not parse a semantic version from pod output: %s", strings.TrimSpace(string(out)))
		}
		targetCanonical := canonicalChartVersion(targetVersion)
		if targetCanonical == "" {
			return "", fmt.Errorf("target version %q is not a valid semantic version", targetVersion)
		}
		if semver.Compare(runningVersion, targetCanonical) != 0 {
			// Version mismatch is a definitive failure — do not retry.
			return "", fmt.Errorf("expected %s but pod reports %s", displayVersion(targetVersion), runningVersion)
		}
		return runningVersion, nil
	}
	return "", fmt.Errorf("upgrade applied but unverified — verify manually: "+
		"kubectl exec -n %s deploy/oberth -- /usr/local/bin/oberth version (%w)", cfg.Namespace, lastErr)
}

func printUpgradeDryRun(w io.Writer, cfg UpgradeConfig, result UpgradeResult, retryFailed bool) (UpgradeResult, error) {
	_, _ = fmt.Fprintf(w, "\nDry-run plan (no cluster changes will be made):\n\n")
	if cfg.WaitIdle {
		_, _ = fmt.Fprintf(w, "  Wait for %s runs in the selected live daemon queue (--wait-idle; --wait-timeout=%s).\n", upgradeWaitSelection(cfg), cfg.WaitTimeout)
	}
	_, _ = fmt.Fprintln(w, "  Verify Helm >= 3.14, read installed values (including defaults), pin compatibility identities, and validate stored values against the target chart schema before applying changes.")
	if cfg.serverPKI {
		_, _ = fmt.Fprintln(w, "  Merge server PKI role names before rollout; replace the CA only when needed and refresh the tunnel CA pin. Other clients must refresh their trust anchor if the CA changes.")
	}
	step := 1

	chart := cfg.ChartOverride
	if chart == "" {
		_, _, chartName, _ := releaseverify.Names(canonicalChartVersion(cfg.BinaryVersion))
		chart = "<verified " + chartName + ">"
		cfg.verifiedChart = true
		source := "fetch from " + displayOr(cfg.ReleaseOrigin, releaseverify.DefaultReleaseOrigin) + " and " + displayOr(cfg.ChartRepository, releaseverify.DefaultChartRepository)
		if cfg.ReleaseDir != "" {
			source = "read from " + cfg.ReleaseDir
		}
		_, _ = fmt.Fprintf(w, "  %d. Verify the signed release %s (%s): release.json and the chart against the compiled-in release key,\n", step, displayVersion(cfg.BinaryVersion), source)
		_, _ = fmt.Fprintln(w, "     the chart against the record's digest, and pin the record's server image digest")
		_, _ = fmt.Fprintln(w)
		step++
	}

	if cfg.NoBackup {
		_, _ = fmt.Fprintf(w, "  %d. No pre-upgrade backup (--no-backup)\n\n", step)
	} else {
		_, _ = fmt.Fprintf(w, "  %d. Take and verify a history-only backup and write the rollback record (oberth upgrade --rollback FILE)\n\n", step)
	}
	step++

	switch {
	case retryFailed:
		_, _ = fmt.Fprintf(w, "  %d. Retry FAILED Helm release at %s (same-version forward repair)\n", step, displayVersion(result.TargetVersion))
	case result.PreviousVersion != "" && canonicalChartVersion(result.PreviousVersion) == canonicalChartVersion(result.TargetVersion):
		if upgradeHasTLSFlags(cfg) {
			_, _ = fmt.Fprintf(w, "  %d. Reconcile server PKI names at %s\n", step, displayVersion(result.TargetVersion))
		} else if len(cfg.UserValues) > 0 || len(cfg.UserSet) > 0 {
			_, _ = fmt.Fprintf(w, "  %d. Re-apply --values/--set at %s\n", step, displayVersion(result.TargetVersion))
		} else {
			_, _ = fmt.Fprintf(w, "  %d. Reconcile the Argo controller profile at %s\n", step, displayVersion(result.TargetVersion))
		}
	default:
		_, _ = fmt.Fprintf(w, "  %d. Upgrade Oberth from %s to %s\n", step, displayVersion(result.PreviousVersion), displayVersion(result.TargetVersion))
	}
	// Render from the same args builder the real path uses so the dry-run
	// plan is always truthful. The image ref is unknown at dry-run time
	// (chart values haven't been fetched) so a placeholder is shown.
	args, flags := upgradeHelmArgs(cfg, chart, "<chart-default>", backupCompatibility{SchemaIdentity: "<installed-schema>", AuditDomain: "<installed-audit>", WitnessKeyInfo: "<installed-witness>"})
	args = append(args, flags...)
	_, _ = fmt.Fprintf(w, "     helm %s\n\n", strings.Join(args, " "))
	step++

	_, _ = fmt.Fprintf(w, "  %d. Wait for deployment rollout completion\n\n", step)
	step++

	_, _ = fmt.Fprintf(w, "  %d. Verify running version matches %s\n\n", step, displayVersion(result.TargetVersion))

	_, _ = fmt.Fprintln(w, "No cluster changes were made (--dry-run).")
	return result, nil
}

func upgradeWaitSelection(cfg UpgradeConfig) string {
	if cfg.WaitFor == "critical" {
		return "critical"
	}
	return "all"
}

// waitForUpgradeIdle observes the selected Deployment through its private admin
// socket, preserving the full queue rather than a scoped remote API view.
func waitForUpgradeIdle(ctx context.Context, cfg UpgradeConfig, deps Deps) error {
	limit := cfg.WaitTimeout
	if limit == 0 {
		limit = DefaultUpgradeWaitTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	start := time.Now()
	output := deps.Output
	if output == nil {
		output = io.Discard
	}
	// Only this wait owns these concurrent writes; join the heartbeat before
	// returning so the following upgrade phase has exclusive output ownership.
	writer := &upgradeWaitWriter{output: output}
	if _, err := fmt.Fprintf(writer, "Waiting before upgrade: selection=%s wait-timeout=%s rollout-timeout=%s\n", upgradeWaitSelection(cfg), limit, cfg.Timeout); err != nil {
		return err
	}
	stop, joined := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(joined)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if _, err := fmt.Fprintf(writer, "Still waiting for %s runs; elapsed=%s limit=%s\n", upgradeWaitSelection(cfg), time.Since(start).Truncate(time.Second), limit); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { close(stop); <-joined }()
	args := []string{"exec", "-c", "oberth", "-n", cfg.Namespace, "deploy/oberth", "--", "/usr/local/bin/oberth", "wait-idle", "--timeout=" + limit.String()}
	if cfg.WaitFor == "critical" {
		args = append(args, "--wait-for=critical")
	}
	if deps.ContextName != "" {
		args = append([]string{"--context", deps.ContextName}, args...)
	}
	// Keep child diagnostics separate from heartbeat writes so split messages
	// remain recognizable without retaining unbounded subprocess output.
	childOutput := &upgradeWaitWriter{output: writer}
	var err error
	if deps.RunCommandStream != nil {
		err = deps.RunCommandStream(ctx, childOutput, "kubectl", args...)
	} else if deps.RunCommand != nil {
		var body []byte
		body, err = deps.RunCommand(ctx, nil, "kubectl", args...)
		if len(body) > 0 {
			_, writeErr := childOutput.Write(body)
			err = errors.Join(err, writeErr)
		}
	} else {
		err = DefaultRunCommandStream(ctx, childOutput, "kubectl", args...)
	}
	if err != nil || ctx.Err() != nil || writer.err() != nil {
		cause := err
		if ctx.Err() != nil && !errors.Is(cause, ctx.Err()) {
			cause = errors.Join(cause, ctx.Err())
		}
		cause = errors.Join(cause, writer.err())
		// The local deadline may kill kubectl before the in-pod waiter can
		// print its final error. Retain its last reported probe cause too.
		if ctx.Err() != nil {
			if reason := childOutput.probeFailure(); reason != "" {
				cause = fmt.Errorf("%w; last reported idle probe failure: %s", cause, reason)
			}
		}
		if cfg.WaitFor == "critical" && childOutput.unsupportedFlag() {
			cause = fmt.Errorf("server too old for --wait-for=critical; use --wait-for=all: %w", cause)
		}
		return fmt.Errorf("wait for %s queue via local kubectl exec failed (elapsed=%s limit=%s); upgrade not attempted: %w", upgradeWaitSelection(cfg), time.Since(start).Truncate(time.Millisecond), limit, cause)
	}
	return nil
}

type upgradeWaitWriter struct {
	mu                 sync.Mutex
	output             io.Writer
	failure            error
	diagnosticTail     string
	unsupportedWaitFor bool
	probeLine          string
	lastProbeFailure   string
}

func (w *upgradeWaitWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	const unknownFlag = "flag provided but not defined: -wait-for"
	diagnostic := w.diagnosticTail + string(p)
	w.unsupportedWaitFor = w.unsupportedWaitFor || strings.Contains(diagnostic, unknownFlag)
	if len(diagnostic) >= len(unknownFlag) {
		diagnostic = diagnostic[len(diagnostic)-len(unknownFlag)+1:]
	}
	w.diagnosticTail = strings.Clone(diagnostic)
	// Child stdout/stderr may split diagnostics at any byte. Keep at most one
	// bounded line and one reason; never retain the whole subprocess output.
	for _, part := range strings.SplitAfter(string(p), "\n") {
		w.probeLine += part
		if len(w.probeLine) > 4096 {
			w.probeLine = strings.Clone(w.probeLine[len(w.probeLine)-4096:])
		}
		if strings.HasSuffix(w.probeLine, "\n") {
			if strings.HasPrefix(w.probeLine, "Waiting for ") {
				if _, reason, ok := strings.Cut(w.probeLine, " queue: transient idle probe failure: "); ok {
					reason, _, _ = strings.Cut(reason, "; retrying (elapsed=")
					w.lastProbeFailure = strings.TrimSpace(reason)
				} else if strings.Contains(w.probeLine, " queue: queued=") || strings.Contains(w.probeLine, " queue: busy ") {
					w.lastProbeFailure = ""
				}
			}
			w.probeLine = ""
		}
	}
	n, err := w.output.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	w.failure = errors.Join(w.failure, err)
	return n, err
}
func (w *upgradeWaitWriter) err() error { w.mu.Lock(); defer w.mu.Unlock(); return w.failure }

func (w *upgradeWaitWriter) unsupportedFlag() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.unsupportedWaitFor
}

func (w *upgradeWaitWriter) probeFailure() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastProbeFailure
}
