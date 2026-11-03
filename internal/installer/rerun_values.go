package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// Resolve defaults before the first write. Reused values belong to the operator;
// detection is only a default for a new release.
func resolveInstallRerunValues(ctx context.Context, cfg *Config, deps Deps, cluster ClusterInfo) error {
	ns := cfg.Namespace
	if ns == "" {
		ns = DefaultNamespace
	}
	if deps.RunHelm != nil {
		_, exists, err := findHelmRelease(ctx, deps, "oberth", ns)
		if err != nil {
			return err
		}
		cfg.existingRelease = exists
		if exists {
			cfg.liveInstallValues, err = readHelmValuesMap(ctx, deps, ns)
			if err != nil {
				return err
			}
		}
	}
	if cfg.NetworkPolicy == "auto" {
		if cfg.existingRelease {
			live, ok := valueAt(cfg.liveInstallValues, "networkPolicy", "enabled").(bool)
			if ok && cluster.Engine != "unknown" && live != (cluster.Engine != "k3s") && deps.Output != nil {
				_, _ = fmt.Fprintln(deps.Output, "NOTE: detected network-policy default differs from the live value; keeping it. Use --network-policy=true or false to change it.")
			}
		} else {
			if cluster.NodesUnavailable && cluster.Engine == "unknown" {
				return errors.New("cannot detect network policy without node access; specify --network-policy=true or false")
			}
			cfg.NetworkPolicy = "true"
			if cluster.Engine == "k3s" {
				cfg.NetworkPolicy = "false"
			}
		}
	}
	if !cfg.existingRelease && cfg.ServiceType == "" {
		cfg.ServiceType = "NodePort"
	}
	return nil
}

func valueAt(values map[string]any, path ...string) any {
	var value any = values
	for _, key := range path {
		m, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = m[key]
	}
	return value
}

func checkProducedIdentities(live map[string]any, key string, count int, produceErr error) error {
	previous, _ := valueAt(live, "argo", key).([]any)
	if len(previous) > 0 && (produceErr != nil || count == 0) {
		return fmt.Errorf("cannot safely reconcile existing argo.%s: identity producer failed or returned no identities; refusing shared-identity fallback", key)
	}
	return nil
}

func readHelmValuesMap(ctx context.Context, deps Deps, ns string, revision ...string) (map[string]any, error) {
	args := []string{"get", "values", "oberth", "-n", ns, "--all", "-o", "json"}
	if len(revision) > 0 && revision[0] != "" {
		args = append(args, "--revision", revision[0])
	}
	raw, err := deps.RunHelm(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("read live Helm values: %w", err)
	}
	defer clear(raw)
	var values map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if !json.Valid(raw) || decoder.Decode(&values) != nil {
		return nil, errors.New("cannot decode live Helm values")
	}
	if values == nil {
		values = map[string]any{}
	}
	return values, nil
}

// Use Helm's own merged config and chart defaults, so values files, --set-json,
// escaped keys and reused values have exactly the semantics of the real apply.
// Only paths are printed; values can contain credentials.
func printHelmValueChanges(ctx context.Context, deps Deps, ns string, raw []byte, revision ...string) error {
	var preview struct {
		Config *map[string]any `json:"config"`
		Chart  struct {
			Values map[string]any `json:"values"`
		} `json:"chart"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if !json.Valid(raw) || decoder.Decode(&preview) != nil || preview.Config == nil {
		return errors.New("helm preview did not return its planned configuration; refusing an unreviewable apply")
	}
	live, err := readHelmValuesMap(ctx, deps, ns, revision...)
	if err != nil {
		return err
	}
	planned := preview.Chart.Values
	if planned == nil {
		planned = map[string]any{}
	}
	mergePlannedValues(planned, *preview.Config)
	var changed []string
	changedValuePaths(live, planned, "", &changed)
	sort.Strings(changed)
	if deps.Output != nil {
		_, _ = fmt.Fprintln(deps.Output, "Planned Helm value changes (values omitted):")
		if len(changed) == 0 {
			_, _ = fmt.Fprintln(deps.Output, "  none")
		}
		for _, key := range changed {
			_, _ = fmt.Fprintf(deps.Output, "  %q\n", key)
		}
	}
	return nil
}
func mergePlannedValues(base, overlay map[string]any) {
	for key, value := range overlay {
		if next, ok := value.(map[string]any); ok {
			existing, ok := base[key].(map[string]any)
			if !ok {
				existing = map[string]any{}
			}
			mergePlannedValues(existing, next)
			base[key] = existing
		} else {
			base[key] = value
		}
	}
}
func changedValuePaths(before, after map[string]any, prefix string, changed *[]string) {
	keys := map[string]bool{}
	for key := range before {
		keys[key] = true
	}
	for key := range after {
		keys[key] = true
	}
	for key := range keys {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		old, oldOK := before[key]
		next, nextOK := after[key]
		oldMap, oldIsMap := old.(map[string]any)
		nextMap, nextIsMap := next.(map[string]any)
		if oldIsMap && nextIsMap {
			changedValuePaths(oldMap, nextMap, path, changed)
		} else if oldOK != nextOK || !reflect.DeepEqual(old, next) {
			*changed = append(*changed, path)
		}
	}
}

func explicitInstallValues(cfg Config) bool {
	return cfg.PushBannerURLSet || cfg.PushBannerURL != "" || cfg.AcceptWitnessGenesisSet || cfg.AcceptWitnessGenesis != "" || cfg.ServiceType != "" || cfg.NetworkPolicy == "true" || cfg.NetworkPolicy == "false" || cfg.ArgoVaultCredentialedRole != "" || cfg.ArgoVaultAddress != "" || len(cfg.ValuesFiles) > 0
}
