package main

import (
	"encoding/json"
	"testing"
)

func TestResourceMessageDecodeAndRunMessage(t *testing.T) {
	var run remoteRun
	if err := json.Unmarshal([]byte(`{"display_reason":"server restart, retry needed","resource_message":"2 steps have undeclared CPU/memory requests."}`), &run); err != nil {
		t.Fatal(err)
	}
	if run.DisplayReason != "server restart, retry needed" || displayRunReason(run) != "server restart, retry needed 2 steps have undeclared CPU/memory requests." {
		t.Fatal("separate resource message lost or replaced the reason")
	}
}
