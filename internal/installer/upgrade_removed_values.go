package installer

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

// removedChartValues are chart values that no longer exist. The chart schema
// refuses them; the upgrade refuses earlier, naming each key and how to drop
// it, so a stored value never fails as an opaque Helm schema error and is
// never silently ignored.
var removedChartValues = []string{
	"admission",
	"autoRetryInfrastructureBudget",
	"argo.ciSecretsServiceAccount",
	"argo.ciServiceAccount",
	"argo.containerDiskPrefixes",
	"argo.enabled",
	"argo.defaultWorkflowTimeout",
	"argo.repoWorkflowTimeouts",
	"argo.workflowTimeout",
	"argo.kubevirtCeiling",
	"argo.vault.ciSecretsRole",
	"argo.vmLauncherServiceAccount",
	"argoSync",
	"auditAnchor.rootsKey",
	"gitBackend",
	"gitcache",
	"maxConcurrentJobs",
	"platformProtection",
	"releases",
	"secretCreatePolicy",
	"trustedTests",
	"upstream.createSecrets",
	"upstream.keySecret",
	"upstream.knownHostsSecret",
	"vm",
	"watchTunnel",
}

// removedChartValue returns the removed value that key names or lies under.
func removedChartValue(key string) (string, bool) {
	for _, removed := range removedChartValues {
		if key == removed || strings.HasPrefix(key, removed+".") {
			return removed, true
		}
	}
	return "", false
}

// clearsRemovedValue reports a --set that drops a removed value: exactly the
// removed key, set to null (Helm deletes a null user value).
func clearsRemovedValue(assignment string) bool {
	key, value, _ := strings.Cut(assignment, "=")
	removed, ok := removedChartValue(key)
	return ok && removed == key && value == "null"
}

// checkRemovedUpgradeValues refuses an upgrade whose stored release values,
// --values files or --set flags still set a removed chart value. A stored
// value is dropped by --set <key>=null: under --reset-then-reuse-values Helm
// deletes a stored key that an override sets to null.
func checkRemovedUpgradeValues(cfg UpgradeConfig, stored map[string]any) error {
	sources := map[string]string{}
	for _, removed := range removedChartValues {
		if value, ok := chartValueAt(stored, removed); ok && value != nil {
			sources[removed] = "stored release values"
		}
	}
	for _, path := range cfg.UserValues {
		body, err := os.ReadFile(path) //nolint:gosec // G304: the operator's own --values file.
		if err != nil {
			return fmt.Errorf("read values file %s: %w", path, err)
		}
		var values map[string]any
		if err := yaml.Unmarshal(body, &values); err != nil {
			return fmt.Errorf("parse values file %s: %w", path, err)
		}
		for _, removed := range removedChartValues {
			if _, ok := chartValueAt(values, removed); ok {
				sources[removed] = "--values " + path
			}
		}
	}
	var unstored []string
	for _, assignment := range cfg.UserSet {
		key, _, _ := strings.Cut(assignment, "=")
		removed, ok := removedChartValue(key)
		if !ok {
			continue
		}
		if clearsRemovedValue(assignment) {
			// Helm drops a null only where the stored values hold the key;
			// elsewhere the null stays and the chart schema refuses it.
			if value, ok := chartValueAt(stored, removed); !ok || value == nil {
				unstored = append(unstored, "--set "+assignment)
			}
			delete(sources, removed)
		} else {
			sources[removed] = "--set " + key
		}
	}
	if len(sources) > 0 {
		return removedValuesError(sources)
	}
	if len(unstored) > 0 {
		slices.Sort(unstored)
		return &ErrSchemaValidation{Detail: fmt.Sprintf("%s clears a removed chart value the stored release values do not set; no upgrade applied. "+
			"Drop the flag and delete the key from any --values file (Helm keeps a null it has nothing to remove, and the chart schema refuses it)",
			strings.Join(unstored, ", "))}
	}
	return nil
}

// refuseRemovedReleaseValues refuses an install re-run or a restore, which
// reuse the installed release's values with Helm --reuse-values, while those
// values (its stored values over its chart's defaults) still carry a removed
// chart value. oberth upgrade is the way out: it drops stored ones with the
// flags it names and resets chart defaults to the current chart's.
func refuseRemovedReleaseValues(installed map[string]any, command string) error {
	var keys []string
	for _, removed := range removedChartValues {
		if value, ok := chartValueAt(installed, removed); ok && value != nil {
			keys = append(keys, removed)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	return &ErrSchemaValidation{Detail: fmt.Sprintf("the installed release values set chart values that no longer exist: %s; %s changed nothing. "+
		"Run oberth upgrade first: it names each stored one with the --set <key>=null flag that drops it, and replaces the installed chart's defaults",
		strings.Join(keys, ", "), command)}
}

func removedValuesError(sources map[string]string) error {
	keys := make([]string, 0, len(sources))
	for key := range sources {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	named := make([]string, len(keys))
	clears := make([]string, len(keys))
	for i, key := range keys {
		named[i] = fmt.Sprintf("%s (%s)", key, sources[key])
		clears[i] = "--set " + key + "=null"
	}
	return &ErrSchemaValidation{Detail: fmt.Sprintf("chart values that no longer exist are set: %s; no upgrade applied. "+
		"Delete them from any --values file and re-run the upgrade with %s (Helm drops a null value from the stored release values)",
		strings.Join(named, ", "), strings.Join(clears, " "))}
}

// dropClearedRemovedValues deletes from the stored values each removed key
// that a --set <key>=null clears, as Helm's --reset-then-reuse-values merge
// does, so the offline render of the stored snapshot matches the upgrade.
func dropClearedRemovedValues(cfg UpgradeConfig, stored map[string]any) {
	for _, assignment := range cfg.UserSet {
		if !clearsRemovedValue(assignment) {
			continue
		}
		key, _, _ := strings.Cut(assignment, "=")
		parent, name := stored, key
		if dot := strings.LastIndex(key, "."); dot >= 0 {
			value, _ := chartValueAt(stored, key[:dot])
			parent, _ = value.(map[string]any)
			name = key[dot+1:]
		}
		delete(parent, name)
	}
}

// chartValueAt reads a dotted key path from nested values.
func chartValueAt(values map[string]any, key string) (any, bool) {
	parts := strings.Split(key, ".")
	current := values
	for i, part := range parts {
		value, ok := current[part]
		if !ok {
			return nil, false
		}
		if i == len(parts)-1 {
			return value, true
		}
		if current, ok = value.(map[string]any); !ok {
			return nil, false
		}
	}
	return nil, false
}
