package installer

import (
	"maps"
	"net/url"
	"slices"
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
	v := chosenValues(merge(operatorValues(stored), c.Values), c)
	server := map[string]any{
		"public": map[string]any{"url": c.URL, "ssh_port": c.SSHPort},
		"runtime": map[string]any{
			"controller_namespace": c.Namespace,
			"controller_configmap": "oberth-controller-config",
		},
	}
	v = merge(v, map[string]any{
		"pipeline": map[string]any{"namespace": c.PipelineNamespace},
		"server":   server,
	})
	v, err := storeAddress(v, c)
	if err != nil {
		return nil, err
	}
	v = merge(v, map[string]any{"nodePorts": map[string]any{"https": c.HTTPSNodePort, "ssh": c.SSHNodePort}})
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
func allowedOverrides(v map[string]any) bool {
	for k := range v {
		switch k {
		case "runs", "policies", "storage", "database", "logs", "schedules", "assist", "secretStore":
		default:
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
