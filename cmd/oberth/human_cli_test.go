package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// A verb in the run-ID position is refused locally with the accepted forms,
// instead of reaching the server as a malformed run ID.
func TestVerbInRunIDPositionIsRefused(t *testing.T) {
	t.Setenv("OBERTH_BASE_URL", "https://oberth.example.invalid")
	t.Setenv("OBERTH_TOKEN", "test-token")
	for _, args := range [][]string{{"run", "show"}, {"run", "list"}, {"artifacts", "list"}, {"log", "show"}} {
		var out bytes.Buffer
		err := runCLI(t.Context(), args, strings.NewReader(""), &out)
		if !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "is not a run ID") {
			t.Fatalf("%v: err = %v", args, err)
		}
	}
}

func TestRunAndRunsHelpListSubcommands(t *testing.T) {
	for command, want := range map[string]string{"run": "resources|retry|rerun|cancel|purge|hold", "runs": "oberth runs cancel", "issues": "oberth issues update"} {
		var out bytes.Buffer
		if err := runCLI(t.Context(), []string{command, "--help"}, strings.NewReader(""), &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), want) {
			t.Fatalf("%s --help omits %q:\n%s", command, want, out.String())
		}
	}
}

func TestUsageErrorIsNotDoubled(t *testing.T) {
	err := runIssues(t.Context(), []string{"--bogus"}, &bytes.Buffer{})
	if !errors.Is(err, errUsage) || strings.Count(err.Error(), "usage error") != 1 {
		t.Fatalf("err = %v", err)
	}
}
