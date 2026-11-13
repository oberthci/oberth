package installer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/oberthci/oberth/internal/auditanchor"
	"github.com/oberthci/oberth/internal/store"
)

func requireUpgradeHelmVersion(ctx context.Context, deps Deps) error {
	raw, err := deps.RunHelm(ctx, []string{"version", "--template", "{{.Version}}"})
	if err != nil {
		return fmt.Errorf("cannot check Helm version (upgrade requires Helm >= 3.14.0): %w", err)
	}
	version := strings.TrimSpace(string(raw))
	if !semver.IsValid(version) || semver.Compare(version, "v3.14.0") < 0 {
		return fmt.Errorf("upgrade requires Helm >= 3.14.0 for --reset-then-reuse-values (detected %q); install a supported Helm before retrying", version)
	}
	return nil
}

// These deployment identities may live only in the INSTALLED chart's defaults.
// Neither the new chart's defaults nor a caller override can supply them.
func readUpgradeCompatibility(ctx context.Context, namespace, revision string, deps Deps) (backupCompatibility, error) {
	raw, err := deps.RunHelm(ctx, []string{"get", "values", "oberth", "-n", namespace, "--revision", revision, "--all", "-o", "json"})
	if err != nil {
		return backupCompatibility{}, fmt.Errorf("cannot read installed compatibility identities with helm get values --all: %w", err)
	}
	var values struct {
		Compatibility struct {
			SchemaIdentity string `json:"schemaIdentity"`
			AuditDomain    string `json:"auditDomain"`
			WitnessKeyInfo string `json:"witnessKeyInfo"`
		} `json:"compatibility"`
	}
	if json.Unmarshal(raw, &values) != nil {
		return backupCompatibility{}, fmt.Errorf("cannot decode installed compatibility identities; refusing to use target chart defaults")
	}
	compat := backupCompatibility{SchemaIdentity: values.Compatibility.SchemaIdentity, AuditDomain: values.Compatibility.AuditDomain, WitnessKeyInfo: values.Compatibility.WitnessKeyInfo}
	if (store.ProtocolConfig{SchemaIdentity: compat.SchemaIdentity, AuditDomain: compat.AuditDomain}).Validate() != nil || auditanchor.ValidateWitnessKeyInfo(compat.WitnessKeyInfo) != nil {
		return backupCompatibility{}, fmt.Errorf("installed compatibility identities are missing or invalid; refusing to use target chart defaults. Releases older than v0.17.5 lack compatibility values: first reconcile the installed release with explicit compatibility.* values verified against the existing database and witness configuration, then retry")
	}
	return compat, nil
}

// Helm upgrades reuse the deployed revision, even when the latest attempt failed.
// Resolve it once so the compatibility and user-value reads describe that same release.
func deployedUpgradeRevision(ctx context.Context, namespace string, deps Deps) (string, error) {
	raw, err := deps.RunHelm(ctx, []string{"history", "oberth", "-n", namespace, "-o", "json"})
	if err != nil {
		return "", fmt.Errorf("read deployed release history before upgrade: %w", err)
	}
	var history []struct {
		Revision int    `json:"revision"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal(raw, &history); err != nil {
		return "", fmt.Errorf("decode deployed release history before upgrade: %w", err)
	}
	revision := 0
	for _, entry := range history {
		if entry.Status == "deployed" && entry.Revision > revision {
			revision = entry.Revision
		}
	}
	if revision == 0 {
		return "", fmt.Errorf("no deployed Helm revision found; refusing to reuse a failed release's configuration")
	}
	return strconv.Itoa(revision), nil
}

type preparedUpgradeValues struct {
	pki            *upgradeServerPKI
	revision       string
	compatibility  backupCompatibility
	storedSnapshot map[string]any
}

func readUpgradeStoredValues(ctx context.Context, namespace, revision string, deps Deps) (map[string]any, error) {
	raw, err := deps.RunHelm(ctx, []string{"get", "values", "oberth", "-n", namespace, "--revision", revision, "-o", "json"})
	if err != nil {
		return nil, fmt.Errorf("read stored release configuration before upgrade: %w", err)
	}
	var values map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber() // The applied snapshot must preserve exact stored integers.
	if !json.Valid(raw) || decoder.Decode(&values) != nil {
		return nil, fmt.Errorf("cannot decode stored release configuration before upgrade")
	}
	if values == nil {
		values = map[string]any{}
	}
	if deps.Output != nil {
		describeStoredPolicy(deps.Output, values)
		_, _ = fmt.Fprintln(deps.Output, "NetworkPolicy capability: unverified (not probed during upgrade maintenance); run install --verify for an explicit security check.")
	}

	return values, nil
}

// Helm owns schema evaluation (including local refs and additionalProperties).
// Validate only reused USER configuration here, not old defaults for keys that
// no longer exist. Unknown keys are reported and refused; never silently prune
// administrator configuration or print a rendered manifest into the plan/log.
func prepareUpgradeValues(ctx context.Context, cfg UpgradeConfig, deps Deps, chart, imageRef string) (preparedUpgradeValues, error) {
	revision, err := deployedUpgradeRevision(ctx, cfg.Namespace, deps)
	if err != nil {
		return preparedUpgradeValues{}, err
	}
	compat, err := readUpgradeCompatibility(ctx, cfg.Namespace, revision, deps)
	if err != nil {
		return preparedUpgradeValues{}, err
	}
	values, err := readUpgradeStoredValues(ctx, cfg.Namespace, revision, deps)
	if err != nil {
		return preparedUpgradeValues{}, err
	}
	var storedSnapshot map[string]any
	if dropRetiredInstallerValues(values, deps.Output) {
		storedSnapshot = values
	}
	if err := checkUpgradePKIKeyConflicts(cfg, values); err != nil {
		return preparedUpgradeValues{}, err
	}
	pki, err := prepareUpgradeServerPKI(ctx, &cfg, deps, values)
	if err != nil {
		return preparedUpgradeValues{}, err
	}
	body, err := json.Marshal(values)
	if err != nil {
		return preparedUpgradeValues{}, err
	}
	directory, err := os.MkdirTemp("", "oberth-upgrade-values-")
	if err != nil {
		return preparedUpgradeValues{}, err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	path := filepath.Join(directory, "stored-values.json")
	if err := os.WriteFile(path, body, 0600); err != nil {
		return preparedUpgradeValues{}, err
	}
	_, flags := upgradeHelmArgs(cfg, chart, imageRef, compat)
	args := []string{"template", "oberth", chart, "-n", cfg.Namespace, "--is-upgrade", "--values", path}
	// Use the exact upgrade pins and operator overrides, in the same order.
	for i := 0; i < len(flags); i++ {
		switch flags[i] {
		case "--reset-then-reuse-values", "--wait":
		case "--timeout":
			i++
		case "--values":
			if flags[i+1] != path {
				args = append(args, flags[i], flags[i+1])
			}
			i++
		case "--set", "--set-string", "--set-json", "--version":
			args = append(args, flags[i], flags[i+1])
			i++
		default:
			return preparedUpgradeValues{}, fmt.Errorf("unexpected generated upgrade flag %q", flags[i])
		}
	}
	if _, err := deps.RunHelm(ctx, args); err != nil {
		return preparedUpgradeValues{}, &ErrSchemaValidation{Detail: fmt.Sprintf("stored configuration/overrides do not render against the target chart; no upgrade applied. Export installed values with helm get values --all, remove/migrate unsupported keys in an explicit values file, and reconcile with helm upgrade --reset-values --values CLEANED_FILE (preserving compatibility.*) before retrying: %v", err)}
	}
	return preparedUpgradeValues{pki: pki, revision: revision, compatibility: compat, storedSnapshot: storedSnapshot}, nil
}

// Keep the cleaned snapshot alive for both the server preview and actual apply.
func (prepared preparedUpgradeValues) configureStoredSnapshot(cfg *UpgradeConfig) (func(), error) {
	if prepared.storedSnapshot == nil {
		return func() {}, nil
	}
	work, err := os.MkdirTemp("", "oberth-upgrade-stored-")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(work) }
	cfg.storedValuesFile, err = writeStoredValuesSnapshot(work, prepared.storedSnapshot)
	if err != nil {
		cleanup()
		return nil, err
	}
	return cleanup, nil
}

func prepareUpgradeDryRunValues(ctx context.Context, cfg *UpgradeConfig, deps Deps) error {
	revision, err := deployedUpgradeRevision(ctx, cfg.Namespace, deps)
	if err != nil {
		return err
	}
	values, err := readUpgradeStoredValues(ctx, cfg.Namespace, revision, deps)
	if err != nil {
		return err
	}
	if dropRetiredInstallerValues(values, deps.Output) {
		cfg.storedValuesFile = "<cleaned-stored-values>"
	}
	if err := checkUpgradePKIKeyConflicts(*cfg, values); err != nil {
		return err
	}
	_, err = prepareUpgradeServerPKI(ctx, cfg, deps, values)
	return err
}
