package main

import (
	"testing"
	"time"
)

func TestRepoWorkflowDeadlineCaps(t *testing.T) {
	got, err := parseRepoWorkflowTimeouts([]string{"forge/org/repo=5m", "forge/org/other=1h"})
	if err != nil || got["forge/org/repo"] != 5*time.Minute || got["forge/org/other"] != time.Hour {
		t.Fatalf("caps=%v err=%v", got, err)
	}
	for _, value := range []string{"repo=1m", "forge/org/repo=0", "forge/org/repo=-1m", "forge/org/repo=1ms", "forge//repo=5m", "forge/org/repo=nope"} {
		if _, err := parseRepoWorkflowTimeouts([]string{value}); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	if _, err := parseRepoWorkflowTimeouts([]string{"forge/org/repo=1m", "forge/org/repo=2m"}); err == nil {
		t.Fatal("duplicate cap accepted")
	}
}
