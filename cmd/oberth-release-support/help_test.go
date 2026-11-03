package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestReleaseSupportHelpMatchesReleaseActions(t *testing.T) {
	var out strings.Builder
	if err := run(context.Background(), []string{"--help"}, &out); err != nil {
		t.Fatalf("help: %v", err)
	}
	actions := regexp.MustCompile(`ACTION \(([^)]+)\)`).FindStringSubmatch(out.String())
	if len(actions) != 2 {
		t.Fatalf("missing bounded heartbeat action list: %s", &out)
	}
	body, err := os.ReadFile(filepath.Join("..", "..", ".oberth", "release.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range strings.Split(actions[1], "|") {
		command, err := releaseActionCommand(t.Context(), action)
		if err != nil || command == nil {
			t.Errorf("advertised action %s refused: %v", action, err)
			continue
		}
		// Inspect only; never execute a release or use credentials in this check.
		if !regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(action) + `\)`).Match(body) {
			t.Errorf("advertised heartbeat action %s is absent from release.sh", action)
		}
	}
	for _, retired := range []string{"publish-r2", "validate", "sh"} {
		if command, err := releaseActionCommand(t.Context(), retired); err == nil || command != nil {
			t.Errorf("unsupported heartbeat action %s accepted", retired)
		}
	}
}
