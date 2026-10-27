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

	"golang.org/x/mod/semver"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// canonicalGARPrefix is the canonical Google Artifact Registry prefix for
// Oberth release images. Published chart refs must resolve to this registry.
const canonicalGARPrefix = "europe-west4-docker.pkg.dev/skipopsmain/oberth/"

// watchTunnelImageDefault is the digest-pinned cloudflared image the chart
// and installer pin by default. Update deliberately.
const watchTunnelImageDefault = "docker.io/cloudflare/cloudflared:2026.9.3@sha256:072c067d25ccbe61d46e18f0d0723255f2bb5304f7317caa95b27031520ff92c"

// watchTunnelOpenbaoImageDefault is the digest-pinned OpenBao image used by
// the init container that fetches the connector token.
const watchTunnelOpenbaoImageDefault = "quay.io/openbao/openbao:2.6.1@sha256:5b2486ab0fb90bbc788cc345b0a08616dfb375873ee8be5df3a2fd4d378a67e0"

// DefaultUpgradeWaitTimeout bounds queue observation independently of rollout.
const DefaultUpgradeWaitTimeout = 2 * time.Hour

// UpgradeConfig holds options for the upgrade command.
type UpgradeConfig struct {
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
}

// ErrPinnedKeyOverride is returned when a user --set or --values file
// attempts to override a value the upgrade path pins for safety.
type ErrPinnedKeyOverride struct {
	Key string
}

func (e *ErrPinnedKeyOverride) Error() string {
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
	"watchTunnel.image":            true,
	"watchTunnel.openbaoImage":     true,
	"compatibility.schemaIdentity": true,
	"compatibility.auditDomain":    true,
	"compatibility.witnessKeyInfo": true,
}

// UpgradePinnedKeys returns the set of pinned keys for tests.
func UpgradePinnedKeys() map[string]bool {
	out := make(map[string]bool, len(upgradePinnedKeys))
	for k, v := range upgradePinnedKeys {
		out[k] = v
	}
	return out
}

// UpgradeResult describes the outcome of an upgrade.
type UpgradeResult struct {
	PreviousVersion string
	TargetVersion   string
	AlreadyUpToDate bool
	Upgraded        bool
}

// ValidateUpgrade checks UpgradeConfig for invalid flag combinations and
// applies defaults.
func (cfg *UpgradeConfig) ValidateUpgrade() error {
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
	for _, kv := range userSet {
		key, err := upgradeSetKey(kv)
		if err != nil {
			return err
		}
		if isPinnedUpgradeKey(key) {
			return &ErrPinnedKeyOverride{Key: key}
		}
	}
	for _, path := range userValues {
		data, err := os.ReadFile(path) //nolint:gosec // G304: path is user-supplied --values flag, intentionally read
		if err != nil {
			return fmt.Errorf("read values file %s: %w", path, err)
		}
		if err := checkValuesPinnedConflicts(data); err != nil {
			return err
		}
	}
	return nil
}

// isPinnedUpgradeKey reports whether key (a dotted Helm value path) conflicts
// with an installer-pinned key: either an exact match or a parent path that
// would wipe a pinned subtree.
func isPinnedUpgradeKey(key string) bool {
	if upgradePinnedKeys[key] {
		return true
	}
	for pinned := range upgradePinnedKeys {
		if strings.HasPrefix(pinned, key+".") {
			return true
		}
	}
	return false
}

// checkValuesPinnedConflicts checks parsed YAML data for leaf keys that
// collide with a pinned upgrade key.
func checkValuesPinnedConflicts(data []byte) error {
	var values map[string]any
	if err := yaml.Unmarshal(data, &values); err != nil {
		return fmt.Errorf("parse values YAML: %w", err)
	}
	leafPaths := make(map[string]bool)
	collectLeafKeyPaths(values, "", leafPaths)
	for path := range leafPaths {
		if isPinnedUpgradeKey(path) {
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
		if !keyPathExists(values, key) {
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
	release, exists := findHelmRelease(ctx, deps, "oberth", cfg.Namespace)
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
	if cmp > 0 {
		return result, fmt.Errorf("installed version %s is newer than CLI version %s — upgrade the CLI first",
			displayVersion(currentVersion), displayVersion(targetVersion))
	}

	// Validate user overrides early (no network needed).
	if err := checkPinnedKeyConflicts(cfg.UserSet, cfg.UserValues); err != nil {
		return result, err
	}

	profile, cleanupProfile, err := prepareUpgradeControllerProfile(ctx, cfg, deps)
	if err != nil {
		return result, err
	}
	defer cleanupProfile()

	// Schema migration announcement: query the running server's schema
	// version and compare with the target binary's version.
	if cfg.BinarySchemaVersion > 0 {
		liveSchema, schemaErr := queryRunningSchemaVersion(ctx, deps, cfg)
		if schemaErr == nil && liveSchema > 0 && liveSchema < cfg.BinarySchemaVersion {
			_, _ = fmt.Fprintf(w, "\nSchema migration v%d will run (forward-only; rollback requires a database restore of /data/oberth.sqlite)\n", cfg.BinarySchemaVersion)
			if !IsLocalServer(server) && !cfg.Yes {
				return result, fmt.Errorf("schema migration v%d→v%d requires --yes for non-local targets", liveSchema, cfg.BinarySchemaVersion)
			}
		}
	}

	if cfg.DryRun {
		if profile != nil {
			profile.describe(w, cfg.Timeout)
		}
		return printUpgradeDryRun(w, cfg, result, retryFailed)
	}

	if err := requireUpgradeHelmVersion(ctx, deps); err != nil {
		return result, err
	}

	// Resolve the chart reference. Use the existing Helm repo if no override.
	chart := cfg.ChartOverride
	if chart == "" {
		if err := refreshHelmRepo(ctx, deps, oberthRepoName, OberthHelmRepoURL); err != nil {
			return result, fmt.Errorf("add Oberth helm repo: %w", err)
		}
		chart = oberthRepoName + "/oberth"
	}

	// Resolve the target chart's image ref so the upgrade pins the exact image.
	imageRef, valuesYAML, err := resolveChartImageRef(ctx, deps, chart, targetVersion, cfg.ChartOverride != "")
	if err != nil {
		return result, fmt.Errorf("resolve chart image: %w", err)
	}

	// Validate user --set keys against the chart values schema.
	if err := validateUserSetAgainstValues(valuesYAML, cfg.UserSet); err != nil {
		return result, err
	}

	compatibility, err := prepareUpgradeValues(ctx, cfg, deps, chart, imageRef)
	if err != nil {
		return result, err
	}

	if cfg.WaitIdle {
		if err := waitForUpgradeIdle(ctx, cfg, deps); err != nil {
			return result, err
		}
	}

	if profile != nil {
		if err := profile.apply(ctx, cfg, deps); err != nil {
			return result, fmt.Errorf("argo controller profile transition incomplete: %w", err)
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
	recordServerAudit(ctx, deps, cfg.Namespace, "deploy/oberth", "upgrade.completed", map[string]any{
		"from": currentVersion, "to": targetVersion,
	})
	_, _ = fmt.Fprintf(w, "\nReady — %s\n", oberthWebUIURL)
	return result, nil
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
	args := []string{
		// Merge new defaults below stored configuration and user overrides.
		// --reuse-values replaces the new chart's defaults with the old ones.
		"--reset-then-reuse-values",
		"--set-string", "image.ref=" + imageRef,
		"--set-string", "compatibility.schemaIdentity=" + compatibility.SchemaIdentity,
		"--set-string", "compatibility.auditDomain=" + compatibility.AuditDomain,
		"--set-string", "compatibility.witnessKeyInfo=" + compatibility.WitnessKeyInfo,
		// Keep the existing proxy enablement and mapping from reused values.
		"--set", "argo.goProxy.port=8444",
		// Retain the explicit promotion grant lifetime safety pin (#826).
		"--set", "promotion.grantMaxLifetime=24h",
		// Preserve the administrator's enablement through reused configuration.
		// An absent enabled key renders false; a present value remains subject
		// to the chart's boolean schema. Only the reviewed image pins override
		// stored connector configuration (#671).
		"--set-string", "watchTunnel.image=" + watchTunnelImageDefault,
		"--set-string", "watchTunnel.openbaoImage=" + watchTunnelOpenbaoImageDefault,
	}
	if cfg.ArgoControllerProfile != "" {
		args = append(args, "--set-string", "argo.controllerProfile="+cfg.ArgoControllerProfile)
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
	// Only pin --version for published charts; a local path IS the version.
	if cfg.ChartOverride == "" && cfg.BinaryVersion != "" {
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
			return fmt.Errorf("timeout (%s) waiting for Oberth deployment rollout in namespace %s", cfg.Timeout, cfg.Namespace)
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
// Returns 0 on any error — the caller treats unknown schema as
// "no announcement" rather than blocking the upgrade.
//
// Old binaries that do not support --schema return a usage error;
// the caller falls through to the zero-value path (no announcement).
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
	_, _ = fmt.Fprintf(w, "\nDO NOT run 'helm rollback' — database migrations are forward-only.\nFix the issue and run 'oberth upgrade' again.\n")
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
	step := 1

	chart := cfg.ChartOverride
	if chart == "" {
		chart = oberthRepoName + "/oberth"
		_, _ = fmt.Fprintf(w, "  %d. Add/update Helm repo\n", step)
		_, _ = fmt.Fprintf(w, "     helm repo add --force-update %s %s\n", oberthRepoName, OberthHelmRepoURL)
		_, _ = fmt.Fprintf(w, "     helm repo update %s\n\n", oberthRepoName)
		step++
	}

	switch {
	case retryFailed:
		_, _ = fmt.Fprintf(w, "  %d. Retry FAILED Helm release at %s (same-version forward repair)\n", step, displayVersion(result.TargetVersion))
	case result.PreviousVersion != "" && canonicalChartVersion(result.PreviousVersion) == canonicalChartVersion(result.TargetVersion):
		if len(cfg.UserValues) > 0 || len(cfg.UserSet) > 0 {
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
