package installer

import (
	"encoding/json"
	"fmt"
	"reflect"
)

func reviewNativeChoices(p *Plan, values map[string]any) []string {
	before, _ := inputs(p.release.chart, p.history.Choices.Values, p.choices.Namespace)
	var lines []string
	for _, name := range []string{"resources", "argoNative", "storeNative", "runPolicy"} {
		old, next := before[name], values[name]
		if name == "argoNative" || name == "storeNative" {
			old = nativeResourceChoices(name, old)
			next = nativeResourceChoices(name, next)
		}
		if !reflect.DeepEqual(old, next) {
			raw, _ := json.Marshal(next)
			lines = append(lines, fmt.Sprintf("Native %s: %s; qualify and roll out at drain.", name, raw))
		}
	}
	runs, _ := values["runs"].(map[string]any)
	if priorities, ok := runs["priorityClasses"].(map[string]any); ok && len(priorities) > 0 {
		raw, _ := json.Marshal(priorities)
		lines = append(lines, "Native run classes: "+string(raw)+"; Never, retained on rollback.")
	}
	return lines
}
