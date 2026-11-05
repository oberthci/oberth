package api

import "testing"

func TestFourPriorityClassSchemasAndLegacyInputAliases(t *testing.T) {
	for _, tool := range toolDefinitions() {
		schema := tool["inputSchema"].(map[string]any)
		properties, _ := schema["properties"].(map[string]any)
		value, ok := properties["priority"].(map[string]any)
		if !ok {
			continue
		}
		names := map[string]bool{}
		for _, name := range value["enum"].([]string) {
			names[name] = true
		}
		for _, name := range []string{"low", "standard", "promotion", "high", "normal", "highest"} {
			if !names[name] {
				t.Errorf("%s missing canonical class/input alias %s", tool["name"], name)
			}
		}
	}
}
