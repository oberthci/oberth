package argoworkflow

import (
	"encoding/json"
	"fmt"
	"strings"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

// A missing argument value is diagnosed by Argo. Supplying it only in the
// diagnostic copy lets Argo inspect the remaining arguments and declarations.
func maskNativeArgument(candidate *wf.Workflow, location string) bool {
	if !strings.Contains(location, ".arguments.parameters[") || !strings.HasSuffix(location, ".value") {
		return false
	}
	raw, _ := json.Marshal(candidate)
	var value any
	_ = json.Unmarshal(raw, &value)
	if !fillNativeArgument(value, "", strings.TrimSuffix(location, ".value")) {
		return false
	}
	raw, _ = json.Marshal(value)
	_ = json.Unmarshal(raw, candidate)
	return true
}

func fillNativeArgument(value any, path, target string) bool {
	switch v := value.(type) {
	case map[string]any:
		if path == target {
			if v["value"] == "unverified" {
				return false
			}
			delete(v, "valueFrom")
			v["value"] = "unverified"
			return true
		}
		for _, key := range sortedKeys(v) {
			if fillNativeArgument(v[key], fieldPath(path, key), target) {
				return true
			}
		}
	case []any:
		for i, item := range v {
			if fillNativeArgument(item, nativeItemPath(path, i, item), target) {
				return true
			}
		}
	}
	return false
}

func nativeItemPath(path string, index int, item any) string {
	object, ok := item.(map[string]any)
	if !ok || stringField(object, "name") == "" {
		return fmt.Sprintf("%s[%d]", path, index)
	}
	name := stringField(object, "name")
	if strings.HasSuffix(path, ".templates") || strings.Contains(path, ".steps[") {
		name = fmt.Sprintf("%q", name)
	}
	return path + "[" + name + "]"
}
