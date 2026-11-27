package installer

import (
	"bytes"
	"encoding/json"
	"io"

	corev1 "k8s.io/api/core/v1"
)

// RunPolicyChoices decodes the explicitly selected namespace policy. Allocation
// units are fixed so Helm can enforce the aggregate ceiling without parsing
// native quantities a second time. LimitRange values remain native quantities.
func RunPolicyChoices(raw string) (map[string]any, error) {
	var policy struct {
		Enabled    bool `json:"enabled"`
		Namespaces map[string]struct {
			CPU       int `json:"cpuMilli"`
			Memory    int `json:"memoryMi"`
			Pods      int `json:"pods"`
			Ephemeral int `json:"ephemeralGi"`
		} `json:"namespaces"`
		DefaultRequest corev1.ResourceList `json:"defaultRequest"`
		Default        corev1.ResourceList `json:"default"`
	}
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if len(raw) > 16384 || decoder.Decode(&policy) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, problem("run-policy", nil)
	}
	var values map[string]any
	_ = json.Unmarshal([]byte(raw), &values)
	if values == nil {
		return nil, problem("run-policy", nil)
	}
	return map[string]any{"runPolicy": values}, nil
}

// RunPriorityChoices exposes only native nonpreempting class values. Chart
// schema binds the suffix and the range below the reserved control class.
func RunPriorityChoices(raw string) (map[string]any, error) {
	var priorities map[string]int
	if len(raw) > 16384 || json.Unmarshal([]byte(raw), &priorities) != nil || priorities == nil {
		return nil, problem("run-policy", nil)
	}
	var values map[string]any
	_ = json.Unmarshal([]byte(raw), &values)
	return map[string]any{"runs": map[string]any{"priorityClasses": values}}, nil
}

func mergeNativeChoices(saved, chosen map[string]any) map[string]any {
	values := merge(saved, chosen)
	// A selected policy map replaces its saved entries; an omitted map keeps
	// them. Null entries reset to chart defaults, as for component resources.
	policy, _ := chosen["runPolicy"].(map[string]any)
	for _, name := range []string{"namespaces", "defaultRequest", "default"} {
		if selected, ok := policy[name]; ok {
			merged, _ := values["runPolicy"].(map[string]any)
			merged[name] = copyValue(selected)
		}
	}
	if values["runPolicy"] == nil {
		delete(values, "runPolicy")
	} else if merged, ok := values["runPolicy"].(map[string]any); ok {
		clearResourceNulls(merged)
	}
	runs, _ := chosen["runs"].(map[string]any)
	if classes, selected := runs["priorityClasses"]; selected {
		merged, _ := values["runs"].(map[string]any)
		merged["priorityClasses"] = copyValue(classes)
	}
	return values
}
