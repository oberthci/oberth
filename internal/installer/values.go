package installer

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"slices"

	"github.com/oberthci/oberth/internal/apicontract"
)

func merge(base, over map[string]any) map[string]any {
	result := copyValues(base)
	if result == nil {
		result = map[string]any{}
	}
	for k, v := range over {
		if m, ok := v.(map[string]any); ok {
			old, _ := result[k].(map[string]any)
			result[k] = merge(old, m)
		} else {
			result[k] = copyValue(v)
		}
	}
	return result
}
func values(r Release, c Choices, stored map[string]any) (map[string]any, error) {
	if !allowedOverrides(c.Values) {
		return nil, problem("choices", nil)
	}
	if c.HTTPSServicePort != defaultHTTPSServicePort && r.chart.Values["servicePort"] == nil {
		return nil, described("service-port-chart", nil, fmt.Sprintf("HTTPS Service port: %d", c.HTTPSServicePort))
	}
	saved := operatorValues(stored)
	if c.replaceValues {
		saved = nil
	}
	v := chosenValues(merge(saved, c.Values), c)
	runtime := map[string]any{
		"controller_namespace": c.Namespace,
		"controller_configmap": "oberth-controller-config",
	}
	pins, err := sourcePins(c, stored)
	if err != nil {
		return nil, err
	}
	if pins != nil {
		runtime["source_policy"] = pins
	}
	server := map[string]any{
		"public":  map[string]any{"url": c.URL, "ssh_port": c.SSHPort},
		"runtime": runtime,
	}
	v = merge(v, map[string]any{
		"pipeline": map[string]any{"namespace": c.PipelineNamespace},
		"server":   server,
	})
	v, err = storeAddress(v, c)
	if err != nil {
		return nil, err
	}
	v = merge(v, map[string]any{"nodePorts": map[string]any{"https": c.HTTPSNodePort, "ssh": c.SSHNodePort}})
	v["servicePort"] = c.HTTPSServicePort
	v["statusOrigins"] = statusOrigins(c, stored)
	return inputs(r.chart, v, c.Namespace)
}

// Only operator choices survive across releases; signed chart facts replace old defaults.
func operatorValues(stored map[string]any) map[string]any {
	result := map[string]any{}
	for name, value := range stored {
		if allowedOverrides(map[string]any{name: value}) {
			result[name] = value
		}
	}
	return result
}

func allowedOverrideNames() []string {
	return []string{"runs", "policies", "storage", "database", "logs", "schedules", "assist", "secretStore"}
}

func allowedOverrides(v map[string]any) bool {
	for k := range v {
		if !slices.Contains(allowedOverrideNames(), k) {
			return false
		}
	}
	return true
}
func storeAddress(v map[string]any, c Choices) (map[string]any, error) {
	address := c.StoreAddress
	store, _ := v["secretStore"].(map[string]any)
	if address == "" {
		address, _ = store["address"].(string)
	}
	if address == "" {
		settings, _ := v["secretStore"].(map[string]any)
		if settings["bundled"] == false {
			return nil, problem("store-url", nil)
		}
		address = storeURL(c.Namespace)
	}
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !validPort(u.Port()) {
		return nil, problem("store-url", err)
	}
	return merge(v, map[string]any{"secretStore": map[string]any{"address": address}}), nil
}
func chosenValues(v map[string]any, c Choices) map[string]any {
	if c.Parallel != nil {
		v = merge(v, map[string]any{"runs": map[string]any{"parallel": *c.Parallel}})
	}
	if c.PodFloor != nil {
		v = merge(v, map[string]any{"policies": map[string]any{"floor": *c.PodFloor}})
	}
	if c.Egress != nil {
		v = merge(v, map[string]any{"policies": map[string]any{"egress": *c.Egress}})
	}
	if c.Store != nil {
		v = merge(v, map[string]any{"secretStore": map[string]any{"bundled": *c.Store}})
	}
	return v
}

func copyValues(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	snapshot := maps.Clone(values)
	for k, v := range snapshot {
		snapshot[k] = copyValue(v)
	}
	return snapshot
}
func copyValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return copyValues(v)
	case map[string]string:
		return maps.Clone(v)
	case []any:
		if v == nil {
			return v
		}
		snapshot := make([]any, len(v))
		for i, item := range v {
			snapshot[i] = copyValue(item)
		}
		return snapshot
	case []string:
		return slices.Clone(v)
	case []byte:
		return slices.Clone(v)
	default:
		return value
	}
}

func statusOrigins(c Choices, stored map[string]any) map[string]any {
	origins := inheritedStatusOrigins(stored)
	if c.statusOrigins != nil {
		origins = maps.Clone(c.statusOrigins)
	}
	for _, name := range c.statusSelected {
		origins[name] = "chosen"
	}
	values := map[string]any{}
	for name, origin := range origins {
		values[name] = origin
	}
	return values
}

func inheritedStatusOrigins(stored map[string]any) map[string]string {
	raw, _ := json.Marshal(stored["statusOrigins"])
	origins := map[string]string{}
	_ = json.Unmarshal(raw, &origins)
	if origins == nil {
		origins = map[string]string{}
	}
	if len(stored) == 0 {
		for _, name := range apicontract.InstallationChoiceNames() {
			origins[name] = "default"
		}
	}
	return origins
}

func explicitStatusChoices(c Choices) []string {
	chosen := scalarStatusChoices(c)
	for name, selected := range map[string]bool{
		apicontract.SettingParallelRuns: c.Parallel != nil, apicontract.SettingPodDefaults: c.PodFloor != nil,
		apicontract.SettingLinkLocalFilter: c.Egress != nil,
		apicontract.SettingSourcePins:      c.ClearSourcePins || c.SourcePins != nil,
		apicontract.SettingServerURL:       c.URL != "", apicontract.SettingStoreMode: c.Store != nil || c.StoreAddress != "",
	} {
		if selected {
			chosen = append(chosen, name)
		}
	}
	return append(chosen, explicitValueSettings(c)...)
}

func (c *Choices) captureStatusChoices() {
	c.statusSelected = nil
	if c.statusOrigins == nil {
		c.statusSelected = explicitStatusChoices(*c)
	}
}

func explicitValueSettings(c Choices) []string {
	chosen := []string{}
	for name, path := range map[string][2]string{
		apicontract.SettingParallelRuns: {"runs", "parallel"}, apicontract.SettingPodDefaults: {"policies", "floor"}, apicontract.SettingLinkLocalFilter: {"policies", "egress"},
		apicontract.SettingLogBytes: {"logs", "run_limit_bytes"}, apicontract.SettingScheduleEntries: {"schedules", "max_entries"}, apicontract.SettingAssistTTL: {"assist", "ttl_seconds"},
		apicontract.SettingStoreMode: {"secretStore", "bundled"}, apicontract.SettingDatabaseFile: {"database", "file"},
		apicontract.SettingDataSize: {"storage", "size"}, apicontract.SettingTmpSize: {"storage", "tmp"},
	} {
		if object, ok := c.Values[path[0]].(map[string]any); ok {
			if _, selected := object[path[1]]; selected {
				chosen = append(chosen, name)
			}
		}
	}
	return chosen
}

func scalarStatusChoices(c Choices) []string {
	names := []string{}
	for name, selected := range map[string]bool{
		apicontract.SettingNamespace: c.Namespace != "", apicontract.SettingPipelineNamespace: c.PipelineNamespace != "",
		apicontract.SettingHTTPSNodePort: c.HTTPSNodePort != 0, apicontract.SettingSSHNodePort: c.SSHNodePort != 0, apicontract.SettingSSHPort: c.SSHPort != 0,
	} {
		if selected {
			names = append(names, name)
		}
	}
	return names
}
