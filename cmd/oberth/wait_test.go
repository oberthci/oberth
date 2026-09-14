package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// scriptedRuns serves a different /api/runs payload on each request, so a test
// can walk a run through queued, running and a verdict.
func scriptedRuns(t *testing.T, payloads []string) *httptest.Server {
	t.Helper()
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		index := int(calls.Add(1)) - 1
		if index >= len(payloads) {
			index = len(payloads) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payloads[index]))
	}))
	t.Cleanup(server.Close)
	return server
}

func run(id, sha, status string) string {
	return `{"ID":"` + id + `","Ref":"main","SHA":"` + sha + `","Status":"` + status + `","Trigger":"push"}`
}

// A wait that stops at the first answer reports a green run before a step has
// started. queued and running are not verdicts.
func TestWaitLoopsPastQueuedAndRunning(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	configure(t, scriptedRuns(t, []string{
		`[` + run("aaaabbbbccccddddeeeeffff00001111", sha, "queued") + `]`,
		`[` + run("aaaabbbbccccddddeeeeffff00001111", sha, "running") + `]`,
		`[` + run("aaaabbbbccccddddeeeeffff00001111", sha, "passed") + `]`,
	}))
	var out bytes.Buffer
	if err := runWait(context.Background(), []string{sha, "--timeout", "30s"}, &out); err != nil {
		t.Fatalf("wait on a run that passes: %v", err)
	}
	body := out.String()
	if !strings.Contains(body, "green") {
		t.Fatalf("no green verdict:\n%s", body)
	}
	// The progress line for each state it passed through, so a long wait is
	// legible rather than silent.
	for _, want := range []string{"queued", "running"} {
		if !strings.Contains(body, want) {
			t.Errorf("progress did not report %q:\n%s", want, body)
		}
	}
}

// A wait that only checks whether the request succeeded exits zero on a red
// run, which is the failure that let a broken push be reported as shipped.
func TestWaitExitsNonZeroOnARedRun(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	payload := `[{"ID":"aaaabbbbccccddddeeeeffff00001111","Ref":"main","SHA":"` + sha +
		`","Status":"failed","Trigger":"push","FailedBurn":"ci","FailedStep":"lint","Error":"biome found 3 errors"}]`
	configure(t, scriptedRuns(t, []string{payload}))

	var out bytes.Buffer
	err := runWait(context.Background(), []string{sha, "--timeout", "30s"}, &out)
	if err == nil {
		t.Fatal("a red run must exit non-zero")
	}
	if !errors.Is(err, errRunFailed) {
		t.Fatalf("error = %v, want errRunFailed so no second line is printed", err)
	}
	body := out.String()
	for _, want := range []string{"failed", "ci/lint", "biome found 3 errors", "oberth log"} {
		if !strings.Contains(body, want) {
			t.Errorf("the verdict is missing %q:\n%s", want, body)
		}
	}
}

// An interrupted run is terminal too, and it is not green.
func TestWaitTreatsInterruptedAsTerminalAndNotGreen(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	configure(t, scriptedRuns(t, []string{`[` + run("aaaabbbbccccddddeeeeffff00001111", sha, "interrupted") + `]`}))
	var out bytes.Buffer
	if err := runWait(context.Background(), []string{sha, "--timeout", "30s"}, &out); err == nil {
		t.Fatal("an interrupted run must exit non-zero")
	}
	if strings.Contains(out.String(), "green") {
		t.Fatalf("an interrupted run was called green:\n%s", out.String())
	}
}

// Expiring must say the run is still going, not that it failed.
func TestWaitTimesOutWithoutClaimingAVerdict(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	configure(t, scriptedRuns(t, []string{`[` + run("aaaabbbbccccddddeeeeffff00001111", sha, "running") + `]`}))
	var out bytes.Buffer
	err := runWait(context.Background(), []string{sha, "--timeout", "1s"}, &out)
	if err == nil {
		t.Fatal("a timeout must exit non-zero")
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Fatalf("timeout error = %v, want it to say the run is still running", err)
	}
	if strings.Contains(out.String(), "green") {
		t.Fatal("a timeout was reported as green")
	}
}

// A run submitted a moment ago is not listed yet. That is not an error, and
// it is certainly not a verdict.
func TestWaitToleratesARunThatIsNotListedYet(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	configure(t, scriptedRuns(t, []string{
		`[]`,
		`[]`,
		`[` + run("aaaabbbbccccddddeeeeffff00001111", sha, "passed") + `]`,
	}))
	var out bytes.Buffer
	if err := runWait(context.Background(), []string{sha, "--timeout", "30s"}, &out); err != nil {
		t.Fatalf("wait on a run that appears late: %v", err)
	}
	if !strings.Contains(out.String(), "green") {
		t.Fatalf("no green verdict:\n%s", out.String())
	}
}

// The server going away mid-wait is a transient read failure, not a red run.
func TestWaitAbsorbsATransientReadFailure(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[` + run("aaaabbbbccccddddeeeeffff00001111", sha, "passed") + `]`))
	}))
	t.Cleanup(server.Close)
	configure(t, server)

	var out bytes.Buffer
	if err := runWait(context.Background(), []string{sha, "--timeout", "40s"}, &out); err != nil {
		t.Fatalf("wait across a transient failure: %v", err)
	}
	if !strings.Contains(out.String(), "green") {
		t.Fatalf("no green verdict:\n%s", out.String())
	}
}

// Selecting by run ID and by an abbreviation both have to work, because the
// push banner prints an abbreviation and the API indexes the whole thing.
func TestWaitSelectsByRunIDAndByAbbreviation(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	payload := `[` + run("aaaabbbbccccddddeeeeffff00001111", sha, "passed") + `]`
	for _, selector := range []string{"aaaabbbbccccddddeeeeffff00001111", "aaaabbbbcccc", sha, sha[:12]} {
		configure(t, scriptedRuns(t, []string{payload}))
		var out bytes.Buffer
		if err := runWait(context.Background(), []string{selector, "--timeout", "20s"}, &out); err != nil {
			t.Fatalf("wait by %q: %v", selector, err)
		}
		if !strings.Contains(out.String(), "green") {
			t.Errorf("wait by %q produced no verdict:\n%s", selector, out.String())
		}
	}
}

func TestWaitRejectsBadUsage(t *testing.T) {
	configure(t, scriptedRuns(t, []string{`[]`}))
	for _, arguments := range [][]string{nil, {"a", "b"}, {"abc", "--timeout", "0s"}} {
		var out bytes.Buffer
		if err := runWait(context.Background(), arguments, &out); !errors.Is(err, errUsage) {
			t.Errorf("wait %v = %v, want a usage error", arguments, err)
		}
	}
}

func TestWaitStopsWhenTheContextIsCancelled(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	configure(t, scriptedRuns(t, []string{`[` + run("aaaabbbbccccddddeeeeffff00001111", sha, "running") + `]`}))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	if err := runWait(ctx, []string{sha, "--timeout", "5m"}, &out); err == nil {
		t.Fatal("a cancelled wait must not report a verdict")
	}
}

// The poll interval is real seconds in production. The tests exercise the
// loop, not the clock.
func TestMain(m *testing.M) {
	waitPollInterval = 10 * time.Millisecond
	os.Exit(m.Run())
}
