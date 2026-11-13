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

// Off the pod an in-pod command says where to run it instead of printing
// SQLite's "unable to open database file".
func TestInPodCommandOffPodNamesWhereToRunIt(t *testing.T) {
	missing := t.TempDir() + "/absent/oberth.sqlite"
	for command, remote := range map[string]string{"schedules": "oberth schedules list", "fragments list": "no remote form"} {
		arguments := append(strings.Fields(command), "--database", missing)
		err := withInPodHint(arguments, runCLI(t.Context(), arguments, strings.NewReader(""), &bytes.Buffer{}))
		var hint inPodCommandError
		if !errors.As(err, &hint) {
			t.Fatalf("%s: err = %v", command, err)
		}
		for _, want := range []string{"oberth " + command + " could not open its local database", "authenticated server API", remote} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s: message omits %q:\n%v", command, want, err)
			}
		}
	}
	if err := withInPodHint([]string{"runs"}, errors.New("other")); err.Error() != "other" {
		t.Fatalf("other errors changed: %v", err)
	}
}
