package installer

import (
	"bytes"
	"encoding/json"
	"io"

	corev1 "k8s.io/api/core/v1"
)

// ResourceChoices decodes explicit native resources for controls and executor.
// Only these typed resource fields can enter the native dependency values.
func ResourceChoices(raw string) (map[string]any, error) {
	var choices map[string]corev1.ResourceRequirements
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if len(raw) > 16384 || decoder.Decode(&choices) != nil || decoder.Decode(new(any)) != io.EOF || len(choices) == 0 {
		return nil, problem("resource-choices", nil)
	}
	values := map[string]any{}
	var authored map[string]any
	_ = json.Unmarshal([]byte(raw), &authored)
	for name, resources := range choices {
		if !validResourceChoices(resources) {
			return nil, problem("resource-choices", nil)
		}
		native := authored[name]
		switch name {
		case "server":
			values["resources"] = native
		case "controller", "executor":
			values = merge(values, map[string]any{"argoNative": map[string]any{name: map[string]any{"resources": native}}})
		case "store":
			values["storeNative"] = map[string]any{"server": map[string]any{"resources": native}}
		default:
			return nil, problem("resource-choices", nil)
		}
	}
	return values, nil
}

func validResourceChoices(r corev1.ResourceRequirements) bool {
	if len(r.Claims) != 0 {
		return false
	}
	for _, list := range []corev1.ResourceList{r.Requests, r.Limits} {
		for name, quantity := range list {
			if !resourceChoiceName(name) || quantity.Sign() < 0 {
				return false
			}
		}
	}
	for name, request := range r.Requests {
		if limit, ok := r.Limits[name]; ok && request.Cmp(limit) > 0 {
			return false
		}
	}
	return true
}

func resourceChoiceName(name corev1.ResourceName) bool {
	switch name {
	case corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage:
		return true
	}
	return false
}

func nativeResourceChoices(name string, value any) map[string]any {
	result := map[string]any{}
	block, _ := value.(map[string]any)
	components := []string{"controller", "executor"}
	if name == "storeNative" {
		components = []string{"server"}
	}
	for _, component := range components {
		part, _ := block[component].(map[string]any)
		if resources, ok := part["resources"]; ok {
			result[component] = map[string]any{"resources": resources}
		}
	}
	return result
}

func nativeResourcesOnly(name string, value any) bool {
	filtered, _ := json.Marshal(nativeResourceChoices(name, value))
	raw, _ := json.Marshal(value)
	return bytes.Equal(filtered, raw)
}

// Explicit null restores a resource field to the target chart's default rather
// than retaining the saved trial value. The input is already an owned merge.
func clearNativeResourceNulls(values map[string]any) map[string]any {
	if values["resources"] == nil {
		delete(values, "resources")
	} else if r, ok := values["resources"].(map[string]any); ok {
		clearResourceNulls(r)
	}
	for _, name := range []string{"argoNative", "storeNative"} {
		block, _ := values[name].(map[string]any)
		for _, component := range block {
			part, _ := component.(map[string]any)
			if part["resources"] == nil {
				delete(part, "resources")
			} else if r, ok := part["resources"].(map[string]any); ok {
				clearResourceNulls(r)
			}
		}
	}
	return values
}

func clearResourceNulls(values map[string]any) {
	for name, value := range values {
		if value == nil {
			delete(values, name)
		} else if nested, ok := value.(map[string]any); ok {
			clearResourceNulls(nested)
		}
	}
}
