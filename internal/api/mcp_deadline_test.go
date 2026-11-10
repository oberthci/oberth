package api

import (
	"slices"
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
			if property := properties[alias].(map[string]any); property["maximum"] != 600 || property["description"] != "Alias of timeout_s" {
				t.Fatalf("%s schema = %#v", alias, property)
			}
		}
	}
	if found != 2 {
		t.Fatalf("found %d long-poll definitions", found)
	}
}

// Every long poll publishes timeout_s with the same aliases, and the client
// deadline table sends the bound in the spelling every server accepts.
func TestToolTimeoutsSpellTimeoutSOnEveryLongPoll(t *testing.T) {
	t.Parallel()
	want := map[string]string{"wait": "timeout", "promote_status": "timeout", "run_wait": "timeout_s", "events_wait": "timeout_s", "run_hold_exec": "timeout_seconds"}
	timeouts := ToolTimeouts()
	for name, argument := range want {
		timeout, ok := timeouts[name]
		if !ok || timeout.Argument != argument || len(timeout.Aliases) != 2 {
			t.Fatalf("%s timeout = %#v, want argument %s with two aliases", name, timeout, argument)
		}
		spellings := append([]string{timeout.Argument}, timeout.Aliases...)
		for _, spelling := range ToolTimeoutArguments {
			if !slices.Contains(spellings, spelling) {
				t.Fatalf("%s lacks %s: %v", name, spelling, spellings)
			}
		}
	}
	if timeouts["events_wait"].Default.Seconds() != 600 || timeouts["run_hold_exec"].Default.Seconds() != 900 {
		t.Fatalf("defaults = %v %v", timeouts["events_wait"].Default, timeouts["run_hold_exec"].Default)
	}
}
