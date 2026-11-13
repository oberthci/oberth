package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
)

// Resolve defaults before the first write. Reused values belong to the operator;
// detection is only a default for a new release.
func resolveInstallRerunValues(ctx context.Context, cfg *Config, deps Deps, cluster ClusterInfo) error {
	if err := readInstallValues(ctx, cfg, deps); err != nil {
		return err
	}
	if err := resolveArgoExecutorResources(ctx, cfg, deps); err != nil {
		return err
	}
	if cfg.NetworkPolicy == "auto" {
		if !cfg.existingRelease {
			if cluster.NodesUnavailable && cluster.Engine == "unknown" {
				return errors.New("cannot detect network policy without node access; specify --network-policy=true or false")
			}
			cfg.NetworkPolicy = "true"
		}
	}
	live, hasLive := valueAt(cfg.liveInstallValues, "networkPolicy", "enabled").(bool)
	if deps.Output != nil && (cfg.NetworkPolicy == "false" || (cfg.NetworkPolicy == "auto" && hasLive && !live)) {
		_, _ = fmt.Fprintln(deps.Output, "WARNING: pipeline NetworkPolicy is disabled by the explicit or stored configuration; pipeline egress is not isolated by the chart. Use --network-policy=true with an enforcing CNI. k3s includes a NetworkPolicy controller.")
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
		// A key whose value is null and an absent key are the same to Helm:
		// helm get values --all drops a null chart default (such as
		// secretstore.external) that the preview still carries, which listed
		// it as a change nobody requested.
		oldOK = oldOK && old != nil
		nextOK = nextOK && next != nil
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
	if cfg.ClientHost != "" || cfg.ClientSSHPort != "" || cfg.ClientHTTPSPort != "" {
		return true
	}
	return cfg.PushBannerURLSet || cfg.PushBannerURL != "" || cfg.AcceptWitnessGenesisSet || cfg.AcceptWitnessGenesis != "" || cfg.ServiceType != "" || cfg.NetworkPolicy == "true" || cfg.NetworkPolicy == "false" || cfg.ArgoVaultCredentialedRole != "" || cfg.ArgoVaultAddress != "" || len(cfg.ValuesFiles) > 0 || cfg.AssistEnabled
}

// Capability probing must not choose or override an existing release's values.
func readInstallValues(ctx context.Context, cfg *Config, deps Deps) error {
	if cfg.existingValuesRead {
		return nil
	}
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
			if err := refuseRemovedReleaseValues(cfg.liveInstallValues, "oberth install"); err != nil {
				return err
			}
		}
	}
	cfg.existingValuesRead = true
	return nil
}

func effectiveNetworkPolicy(cfg Config) string {
	if cfg.NetworkPolicy != "" && cfg.NetworkPolicy != "auto" {
		return cfg.NetworkPolicy
	}
	if live, ok := valueAt(cfg.liveInstallValues, "networkPolicy", "enabled").(bool); ok {
		if live {
			return "true"
		}
		return "false"
	}
	if cfg.existingRelease {
		return "unspecified (preserved)"
	}
	return "true"
}

func installPolicyDeps(host InstallDeps, cfg Config, selected string) Deps {
	args := []string{"--kube-context", selected}
	if cfg.KubeconfigPath != "" {
		args = append([]string{"--kubeconfig", cfg.KubeconfigPath}, args...)
	}
	return Deps{RunHelm: HelmWithKubeArgs(host.RunHelm, args), Output: host.Output}
}

// Verification describes the installed setting, regardless of desired flags.
func resolveInstalledPolicy(ctx context.Context, cfg *Config, deps Deps) error {
	if err := readInstallValues(ctx, cfg, deps); err != nil {
		return err
	}
	if !cfg.existingRelease {
		return errors.New("installed Helm release not found")
	}
	live, known := valueAt(cfg.liveInstallValues, "networkPolicy", "enabled").(bool)
	if !known {
		return errors.New("effective installed NetworkPolicy setting missing")
	}
	cfg.NetworkPolicy = "false"
	if live {
		cfg.NetworkPolicy = "true"
	}
	return nil
}

func describeStoredPolicy(w io.Writer, values map[string]any) {
	choice := effectiveNetworkPolicy(Config{existingRelease: true, liveInstallValues: values})
	_, _ = fmt.Fprintf(w, "Pipeline NetworkPolicy: %s (stored choice; preserved)\n", choice)
	printPolicyWarning(w, choice)
}
func printPolicyWarning(w io.Writer, choice string) {
	switch choice {
	case "false":
		_, _ = fmt.Fprintln(w, "WARNING: pipeline NetworkPolicy is disabled by the stored configuration; pipeline egress is not isolated by the chart.")
	case "unspecified (preserved)":
		_, _ = fmt.Fprintln(w, "WARNING: stored NetworkPolicy choice is unspecified; maintenance preserves it. Explicit verification remains unverified until an effective boolean is available.")
	}
}
