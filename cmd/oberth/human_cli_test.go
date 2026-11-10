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

func TestStatusShowsTheWholeAdmissionHold(t *testing.T) {
	reason := "release qualification for the storage migration; do not admit branch work until the backup is verified"
	configure(t, remoteServer(t, `{"database":"ok","admission":{"hold":{"on":true,"reason":"`+reason+`","actor":"operator","allow":["tag","release"],"expires_at":"2026-11-04T12:00:00.123456789Z"},"used":0,"runs":0,"reservations":[]}}`))
	var out bytes.Buffer
	if err := runRemoteStatus(t.Context(), nil, &out); err != nil {
		t.Fatal(err)
	}
	want := "admission:     held by admin operator until 2026-11-04T12:00:00Z: " + reason + " (allowed: tag, release)"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("status omits the hold:\n%s", out.String())
	}
	configure(t, remoteServer(t, `{"database":"ok","admission":{"hold":{"on":false,"reason":"old"}}}`))
	out.Reset()
	if err := runRemoteStatus(t.Context(), nil, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "admission:") {
		t.Fatalf("an expired hold is shown:\n%s", out.String())
	}
}
