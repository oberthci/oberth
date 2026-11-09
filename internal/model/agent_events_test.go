package model

import "testing"

func TestSyncLockKeyStripsOnlyTheNamespace(t *testing.T) {
	for input, want := range map[string]string{
		"oberth-argo/ConfigMap/oberth-sync-limits/kvm": "ConfigMap/oberth-sync-limits/kvm",
		"ns/Mutex/oberth-release":                      "Mutex/oberth-release",
		"Mutex/oberth-release":                         "Mutex/oberth-release",
		" ns/Database/x ":                              "Database/x",
		"something/else":                               "something/else",
	} {
		if got := SyncLockKey(input); got != want {
			t.Errorf("SyncLockKey(%q) = %q, want %q", input, got, want)
		}
	}
	if !ValidAgentEventKind(AgentEventRunHeld) || !ValidAgentEventKind(AgentEventRunHoldAbandoned) || ValidAgentEventKind("run.bogus") || len(AgentEventKinds()) != 7 {
		t.Fatal("event kind vocabulary changed")
	}
}
