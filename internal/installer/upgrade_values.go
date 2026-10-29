package installer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
func readUpgradeCompatibility(ctx context.Context, namespace string, deps Deps) (backupCompatibility, error) {
	raw, err := deps.RunHelm(ctx, []string{"get", "values", "oberth", "-n", namespace, "--all", "-o", "json"})
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

type preparedUpgradeValues struct {
	compatibility backupCompatibility
	storedValues  string // cleaned user snapshot; empty keeps ordinary Helm reuse
	cleanup       func()
}

// Helm owns schema evaluation (including local refs and additionalProperties).
// Validate only reused USER configuration here, not old defaults for keys that
// no longer exist. Unknown keys are reported and refused; never silently prune
// administrator configuration or print a rendered manifest into the plan/log.
func prepareUpgradeValues(ctx context.Context, cfg UpgradeConfig, deps Deps, chart, imageRef string) (preparedUpgradeValues, error) {
	compat, err := readUpgradeCompatibility(ctx, cfg.Namespace, deps)
	if err != nil {
		return preparedUpgradeValues{}, err
	}
	raw, err := deps.RunHelm(ctx, []string{"get", "values", "oberth", "-n", cfg.Namespace, "-o", "json"})
	if err != nil {
		return preparedUpgradeValues{}, fmt.Errorf("read stored release configuration before upgrade: %w", err)
	}
	var values map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber() // The applied snapshot must preserve exact stored integers.
	if !json.Valid(raw) || decoder.Decode(&values) != nil {
		return preparedUpgradeValues{}, fmt.Errorf("cannot decode stored release configuration before upgrade")
	}
	if values == nil {
		values = map[string]any{}
	}
	// Only the retired boolean is migrated. Other administrator keys still
	// reach the target schema unchanged; malformed shadow values are refused.
	removedShadow := false
	var legacyShadow bool
	if cache, ok := values["gitcache"].(map[string]any); ok {
		if shadow, present := cache["shadow"]; present {
			value, ok := shadow.(bool)
			if !ok {
				return preparedUpgradeValues{}, &ErrSchemaValidation{Detail: "stored gitcache.shadow must be a legacy boolean; no upgrade applied"}
			}
			legacyShadow = value
			delete(cache, "shadow")
			if len(cache) == 0 {
				delete(values, "gitcache")
			}
			removedShadow = true
		}
	}
	body, err := json.Marshal(values)
	if err != nil {
		return preparedUpgradeValues{}, err
	}
	directory, err := os.MkdirTemp("", "oberth-upgrade-values-")
	if err != nil {
		return preparedUpgradeValues{}, err
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	keepSnapshot := false
	defer func() {
		if !keepSnapshot {
			cleanup()
		}
	}()
	path := filepath.Join(directory, "stored-values.json")
	if err := os.WriteFile(path, body, 0600); err != nil {
		return preparedUpgradeValues{}, err
	}
	if removedShadow {
		cfg.storedValues = path
	}
	_, flags := upgradeHelmArgs(cfg, chart, imageRef, compat)
	args := []string{"template", "oberth", chart, "-n", cfg.Namespace, "--is-upgrade", "--values", path}
	// Use the exact upgrade pins and operator overrides, in the same order.
	for i := 0; i < len(flags); i++ {
		switch flags[i] {
		case "--reset-then-reuse-values", "--reset-values", "--wait":
		case "--timeout":
			i++
		case "--values":
			if flags[i+1] != path {
				args = append(args, flags[i], flags[i+1])
			}
			i++
		case "--set", "--set-string", "--version":
			args = append(args, flags[i], flags[i+1])
			i++
		default:
			return preparedUpgradeValues{}, fmt.Errorf("unexpected generated upgrade flag %q", flags[i])
		}
	}
	if _, err := deps.RunHelm(ctx, args); err != nil {
		return preparedUpgradeValues{}, &ErrSchemaValidation{Detail: fmt.Sprintf("stored configuration/overrides do not render against the target chart; no upgrade applied. Export installed values with helm get values --all, remove/migrate unsupported keys in an explicit values file, and reconcile with helm upgrade --reset-values --values CLEANED_FILE (preserving compatibility.*) before retrying: %v", err)}
	}
	prepared := preparedUpgradeValues{compatibility: compat, cleanup: cleanup}
	if removedShadow {
		prepared.storedValues = path
		if deps.Output != nil {
			_, _ = fmt.Fprintf(deps.Output, "Dropped stored inert legacy gitcache.shadow=%t for this upgrade; the prior Helm revision retains its original value for rollback.\n", legacyShadow)
		}
	}
	keepSnapshot = true
	return prepared, nil
}
