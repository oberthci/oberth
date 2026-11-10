package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
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
	expiry := "2026-11-04T12:00:00Z" // the hold's expires_at, to the second
	want := "admission:     held by admin operator until " + expiry + ": " + reason + " (allowed: tag, release)"
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

// Off the pod an in-pod command says where to run it instead of printing
// SQLite's "unable to open database file".
func TestInPodCommandOffPodNamesWhereToRunIt(t *testing.T) {
	missing := t.TempDir() + "/absent/oberth.sqlite"
	for command, remote := range map[string]string{"uplink list": "oberth mcp call uplink_list", "schedules": "oberth schedules list", "fragments list": "no remote form"} {
		arguments := append(strings.Fields(command), "--database", missing)
		err := withInPodHint(arguments, runCLI(t.Context(), arguments, strings.NewReader(""), &bytes.Buffer{}))
		var hint inPodCommandError
		if !errors.As(err, &hint) {
			t.Fatalf("%s: err = %v", command, err)
		}
		for _, want := range []string{"oberth " + command + " reads the server database", "kubectl exec -n oberth deploy/oberth -- oberth " + command, remote} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s: message omits %q:\n%v", command, want, err)
			}
		}
	}
	if err := withInPodHint([]string{"runs"}, errors.New("other")); err.Error() != "other" {
		t.Fatalf("other errors changed: %v", err)
	}
}

func TestAdmissionShowsTheWholeHold(t *testing.T) {
	var out bytes.Buffer
	state := &model.AdmissionState{Hold: &model.AdmissionHold{On: true, Reason: strings.Repeat("r", 120), Actor: "operator"}}
	if err := renderAdmission(&out, state, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "hold: held by admin operator: "+strings.Repeat("r", 120)+" (allowed: nothing)") {
		t.Fatalf("admission omits the hold:\n%s", out.String())
	}
}
