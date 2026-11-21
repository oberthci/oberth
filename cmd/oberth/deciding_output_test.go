package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecidingFailedStepHintQuotesOneArgument(t *testing.T) {
	data := json.RawMessage(`{"id":"deadbeef","status":"failed","ref":"refs/heads/topic","failure":{"failed_step":"integration suite; echo extra"}}`)
	output, ok := Display("run_get", data)
	if !ok {
		t.Fatal("run display refused valid failure")
	}
	want := "Failed-step logs:\noberth log deadbeef 'integration suite; echo extra'\n"
	if !strings.Contains(output, want) {
		t.Fatalf("failed-step hint=%q want whole line %q", output, want)
	}
}
