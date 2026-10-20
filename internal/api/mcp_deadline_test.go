package api

import (
	"strings"
	"testing"
)

func TestMCPLongPollDescriptionStatesEffectiveMaximum(t *testing.T) {
	t.Parallel()
	found := 0
	for _, definition := range toolDefinitions() {
		if definition["name"] != "wait" && definition["name"] != "promote_status" {
			continue
		}
		found++
		description := definition["description"].(string)
		for _, text := range []string{"600", "rejected", "30 seconds", "--timeout"} {
			if !strings.Contains(description, text) {
				t.Fatalf("%s missing %q: %s", definition["name"], text, description)
			}
		}
		schema := definition["inputSchema"].(map[string]any)
		timeout := schema["properties"].(map[string]any)["timeout"].(map[string]any)
		if timeout["maximum"] != 600 || timeout["default"] != 600 || timeout["minimum"] != 0 {
			t.Fatalf("timeout schema = %#v", timeout)
		}
	}
	if found != 2 {
		t.Fatalf("found %d long-poll definitions", found)
	}
}
