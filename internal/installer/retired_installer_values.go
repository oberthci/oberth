package installer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

// Only these leaves were written by the installer for the removed Plan
// feature. Keep every live setting and every other unknown key for validation.
func dropRetiredInstallerValues(values map[string]any, output io.Writer) bool {
	store, _ := values["secretstore"].(map[string]any)
	transit, _ := store["transit"].(map[string]any)
	var removed []string
	for _, key := range []string{"enabled", "mount", "key"} {
		if _, exists := transit[key]; exists {
			delete(transit, key)
			removed = append(removed, "secretstore.transit."+key)
		}
	}
	if len(removed) == 0 {
		return false
	}
	if len(transit) == 0 {
		delete(store, "transit")
	}
	if output != nil {
		_, _ = fmt.Fprintf(output, "Omitted retired installer-written stored values: %s\n", strings.Join(removed, ", "))
	}
	return true
}

func writeStoredValuesSnapshot(work string, values map[string]any) (string, error) {
	body, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	defer clear(body)
	path := filepath.Join(work, "stored-values.json")
	if err := os.WriteFile(path, body, 0600); err != nil {
		return "", err
	}
	return path, nil
}

// A values overlay cannot remove old chart defaults loaded by --reuse-values.
// Reset to the target defaults and apply the cleaned snapshot before operator
// overrides, preserving their original precedence.
func storedSnapshotHelmArgs(args []string, path string) []string {
	result := []string{args[0], "--reset-values", "--values", path}
	for _, arg := range args[1:] {
		if arg != "--reuse-values" && arg != "--reset-then-reuse-values" {
			result = append(result, arg)
		}
	}
	return result
}

func prepareRetiredInstallerReuse(ctx context.Context, deps Deps, namespace, work string, args []string) ([]string, error) {
	// Reuse only USER values. Coalesced defaults would become permanent
	// operator overrides after Helm stores this snapshot in release.Config.
	raw, err := deps.RunHelm(ctx, []string{"get", "values", "oberth", "-n", namespace, "-o", "json"})
	if err != nil {
		return nil, fmt.Errorf("read stored Helm user values: %w", err)
	}
	defer clear(raw)
	var values map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if !json.Valid(raw) || decoder.Decode(&values) != nil {
		return nil, fmt.Errorf("cannot decode stored Helm user values")
	}
	if !dropRetiredInstallerValues(values, deps.Output) {
		// --reuse-values also loads old defaults, even when no Transit leaf
		// was stored by the operator. Inspect them only to decide on reset;
		// none of these coalesced values may enter the user snapshot.
		installed, err := readHelmValuesMap(ctx, deps, namespace)
		if err != nil {
			return nil, err
		}
		if !dropRetiredInstallerValues(installed, deps.Output) {
			return args, nil
		}
	}
	// These identities may exist only in the installed chart defaults. Keep
	// them in the initial snapshot, below operator files and the restore overlay.
	compat, err := readUpgradeCompatibility(ctx, namespace, "", deps)
	if err != nil {
		return nil, err
	}
	if values == nil {
		values = make(map[string]any)
	}
	values["compatibility"] = map[string]any{
		"schemaIdentity": compat.SchemaIdentity,
		"auditDomain":    compat.AuditDomain,
		"witnessKeyInfo": compat.WitnessKeyInfo,
	}
	path, err := writeStoredValuesSnapshot(work, values)
	if err != nil {
		return nil, err
	}
	return storedSnapshotHelmArgs(args, path), nil
}

// Restore targets the installed chart, which can still own Transit flags.
// Removing its values would reactivate that chart's disabled Transit default.
func targetChartRetainsTransit(ctx context.Context, deps Deps, chart string) (bool, error) {
	raw, err := deps.RunHelm(ctx, []string{"show", "values", chart})
	if err != nil {
		return false, fmt.Errorf("read restore chart defaults: %w", err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(raw, &values); err != nil {
		return false, fmt.Errorf("decode restore chart defaults: %w", err)
	}
	store, _ := values["secretstore"].(map[string]any)
	_, exists := store["transit"]
	return exists, nil
}
