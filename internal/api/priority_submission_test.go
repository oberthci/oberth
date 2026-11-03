package api

import "testing"

func TestSubmissionPrioritySchemasRemainAdditiveObjects(t *testing.T) {
	remaining := map[string]bool{"promote": true, "run_rerun": true, "run_retry": true, "sync": true, "schedule_trigger": true}
	grants := map[string]bool{"priority_grant": true, "priority_grant_list": true, "priority_grant_revoke": true}
	for _, definition := range toolDefinitions() {
		name := definition["name"].(string)
		if !remaining[name] && !grants[name] {
			continue
		}
		schema := definition["inputSchema"].(map[string]any)
		if schema["type"] != "object" || schema["oneOf"] != nil || schema["anyOf"] != nil {
			t.Fatalf("%s must use a plain object input schema: %+v", name, schema)
		}
		if remaining[name] {
			properties := schema["properties"].(map[string]any)
			if properties["priority"] == nil {
				t.Fatalf("%s has no optional priority", name)
			}
			if required, ok := schema["required"].([]string); ok {
				for _, field := range required {
					if field == "priority" {
						t.Fatalf("%s requires optional priority", name)
					}
				}
			}
			delete(remaining, name)
		}
		delete(grants, name)
	}
	if len(remaining) != 0 || len(grants) != 0 {
		t.Fatalf("missing schemas: %v %v", remaining, grants)
	}
}
