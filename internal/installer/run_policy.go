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
