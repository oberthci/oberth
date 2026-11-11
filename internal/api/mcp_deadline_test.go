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
		properties := schema["properties"].(map[string]any)
		timeout := properties["timeout_s"].(map[string]any)
		if timeout["maximum"] != 600 || timeout["default"] != 600 || timeout["minimum"] != 0 {
			t.Fatalf("timeout_s schema = %#v", timeout)
		}
		for _, alias := range []string{"timeout", "timeout_seconds"} {
			if _, present := properties[alias]; present {
				t.Fatalf("%s still publishes the removed %s alias", definition["name"], alias)
			}
		}
	}
	if found != 2 {
		t.Fatalf("found %d long-poll definitions", found)
	}
}

// Every blocking tool publishes timeout_s and no other spelling of its bound.
func TestToolTimeoutsSpellTimeoutSOnEveryLongPoll(t *testing.T) {
	t.Parallel()
	timeouts := ToolTimeouts()
	for _, name := range []string{"wait", "promote_status", "run_wait", "events_wait", "run_hold_exec", "secretstore_verify"} {
		if _, ok := timeouts[name]; !ok {
			t.Fatalf("%s lacks timeout_s", name)
		}
	}
	for _, definition := range toolDefinitions() {
		properties := definition["inputSchema"].(map[string]any)["properties"].(map[string]any)
		for _, removed := range []string{"timeout", "timeout_seconds"} {
			if _, present := properties[removed]; present {
				t.Fatalf("%s publishes %s", definition["name"], removed)
			}
		}
	}
	if timeouts["events_wait"].Default.Seconds() != 600 || timeouts["run_hold_exec"].Default.Seconds() != 900 || timeouts["secretstore_verify"].Default.Seconds() != 45 {
		t.Fatalf("defaults = %v %v %v", timeouts["events_wait"].Default, timeouts["run_hold_exec"].Default, timeouts["secretstore_verify"].Default)
	}
}
