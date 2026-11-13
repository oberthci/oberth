package installer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	values, err := readHelmValuesMap(ctx, deps, namespace)
	if err != nil {
		return nil, err
	}
	// The coalesced --all view omits explicit nulls. Overlay the raw stored
	// user values so reset-values preserves those deletions as well.
	raw, err := deps.RunHelm(ctx, []string{"get", "values", "oberth", "-n", namespace, "-o", "json"})
	if err != nil {
		return nil, fmt.Errorf("read stored Helm user values: %w", err)
	}
	defer clear(raw)
	var user map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if !json.Valid(raw) || decoder.Decode(&user) != nil {
		return nil, fmt.Errorf("cannot decode stored Helm user values")
	}
	mergePlannedValues(values, user)
	if !dropRetiredInstallerValues(values, deps.Output) {
		return args, nil
	}
	path, err := writeStoredValuesSnapshot(work, values)
	if err != nil {
		return nil, err
	}
	return storedSnapshotHelmArgs(args, path), nil
}
