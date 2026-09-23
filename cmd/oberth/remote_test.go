package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func remoteServer(t *testing.T, payload string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(server.Close)
	return server
}

func configure(t *testing.T, server *httptest.Server) {
	t.Helper()
	t.Setenv("OBERTH_BASE_URL", server.URL)
	t.Setenv("OBERTH_TOKEN", "test-token")
	t.Setenv("OBERTH_TOKEN_COMMAND", "")
	t.Setenv("OBERTH_CA_CERT", "")
}

const runsPayload = `[{"ID":"run-abc","Ref":"main","SHA":"0123456789abcdef","Status":"failed","Trigger":"schedule"}]`

func TestRunsRendersTheServersRows(t *testing.T) {
	configure(t, remoteServer(t, runsPayload))
	var out bytes.Buffer
	if err := runRuns(context.Background(), nil, &out); err != nil {
		t.Fatal(err)
	}
	body := out.String()
	for _, want := range []string{"run-abc", "failed", "schedule", "0123456", "main"} {
		if !strings.Contains(body, want) {
			t.Fatalf("output missing %q:\n%s", want, body)
		}
	}
}

func TestRunsJSONEmitsTheServerPayload(t *testing.T) {
	configure(t, remoteServer(t, runsPayload))
	var out bytes.Buffer
	if err := runRuns(context.Background(), []string{"--json"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"Trigger": "schedule"`) {
		t.Fatalf("json output is not the server's own shape:\n%s", out.String())
	}
	if strings.Contains(out.String(), "RUN ") {
		t.Fatalf("the rendered table contaminated --json output:\n%s", out.String())
	}
}

func TestRunDetailMarksTheFailingStep(t *testing.T) {
	payload := `{"Run":{"ID":"run-abc","Status":"failed","Ref":"main","SHA":"0123456789abcdef",` +
		`"FailedBurn":"ci","FailedStep":"test","Actor":"miki","Trigger":"branch"},` +
		`"Steps":[{"Burn":"ci","Step":"lint","Status":"passed"},{"Burn":"ci","Step":"test","Status":"failed"}],` +
		`"Repository":{"Name":"demo","DefaultBranch":"main"}}`
	configure(t, remoteServer(t, payload))
	var out bytes.Buffer
	if err := runRunDetail(context.Background(), []string{"run-abc"}, &out); err != nil {
		t.Fatal(err)
	}
	body := out.String()
	if !strings.Contains(body, "* ci") {
		t.Fatalf("the failing step is not marked:\n%s", body)
	}
	if !strings.Contains(body, "demo") {
		t.Fatalf("repository missing:\n%s", body)
	}
}

func TestReposLists(t *testing.T) {
	configure(t, remoteServer(t, `[{"ID":1,"Name":"alpha","DefaultBranch":"main"}]`))
	var out bytes.Buffer
	if err := runRepos(context.Background(), nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "alpha") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestARemoteCommandWithNoBaseURLNamesTheVariable(t *testing.T) {
	t.Setenv("OBERTH_BASE_URL", "")
	t.Setenv("OBERTH_TOKEN", "test-token")
	var out bytes.Buffer
	err := runRuns(context.Background(), nil, &out)
	if err == nil {
		t.Fatal("a remote command ran with no server configured")
	}
	if !strings.Contains(err.Error(), "OBERTH_BASE_URL") {
		t.Fatalf("error does not name the variable to set: %v", err)
	}
}

func TestRenderedOutputCarriesNoModeLine(t *testing.T) {
	configure(t, remoteServer(t, runsPayload))
	var out bytes.Buffer
	if err := runRuns(context.Background(), nil, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "reading:") {
		t.Fatalf("the mode line went to stdout, so it would contaminate a pipe:\n%s", out.String())
	}
}

func TestLogPassesEveryFilterAsAQueryParameter(t *testing.T) {
	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"run_id":"r","burn":"ci","step":"test","output":"","total_lines":0}`))
	}))
	defer server.Close()
	configure(t, server)

	var out bytes.Buffer
	err := runRemoteLog(context.Background(), []string{
		"--burn", "ci", "--step", "test", "--pattern", "FAIL",
		"--context", "3", "--offset", "5", "--limit", "10", "--tail", "run-abc",
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"burn=ci", "step=test", "pattern=FAIL", "context=3", "offset=5", "limit=10", "tail=true"} {
		if !strings.Contains(seen, want) {
			t.Fatalf("query %q missing %q", seen, want)
		}
	}
}

func TestLogStripsTheStepPrefixUnlessAskedNotTo(t *testing.T) {
	payload := `{"run_id":"r","burn":"ci","step":"test","output":"[ci/test] first\n[ci/test] second\n",` +
		`"total_lines":2,"matched_lines":2,"returned_lines":2}`
	configure(t, remoteServer(t, payload))

	var out bytes.Buffer
	if err := runRemoteLog(context.Background(), []string{"--burn", "ci", "--step", "test", "run-abc"}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "[ci/test]") {
		t.Fatalf("the prefix was not stripped:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "first") {
		t.Fatalf("content lost:\n%s", out.String())
	}

	var raw bytes.Buffer
	if err := runRemoteLog(context.Background(), []string{"--burn", "ci", "--step", "test", "--raw", "run-abc"}, &raw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw.String(), "[ci/test]") {
		t.Fatalf("--raw did not keep the prefix:\n%s", raw.String())
	}
}

func TestLogRequiresBurnAndStep(t *testing.T) {
	configure(t, remoteServer(t, `{}`))
	var out bytes.Buffer
	err := runRemoteLog(context.Background(), []string{"run-abc"}, &out)
	if err == nil {
		t.Fatal("log ran without --burn and --step")
	}
	if !strings.Contains(err.Error(), "--burn") {
		t.Fatalf("error does not say what is missing: %v", err)
	}
}

func TestLogCountsGoToStderrNotStdout(t *testing.T) {
	payload := `{"run_id":"r","burn":"ci","step":"test","output":"line\n","total_lines":9,` +
		`"matched_lines":1,"returned_lines":1,"truncated":true}`
	configure(t, remoteServer(t, payload))

	var out bytes.Buffer
	if err := runRemoteLog(context.Background(), []string{"--burn", "ci", "--step", "test", "run-abc"}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "lines returned") || strings.Contains(out.String(), "TRUNCATED") {
		t.Fatalf("the counts contaminated the log body, so piping to a file would corrupt it:\n%s", out.String())
	}
	if strings.TrimSpace(out.String()) != "line" {
		t.Fatalf("stdout is not just the log:\n%q", out.String())
	}
}

func TestEveryRemoteCommandAcceptsJSON(t *testing.T) {
	configure(t, remoteServer(t, `{"ok":true}`))
	commands := map[string]func(context.Context, []string, io.Writer) error{
		"runs":   runRuns,
		"repos":  runRepos,
		"issues": runIssues,
		"status": runRemoteStatus,
	}
	for name, run := range commands {
		var out bytes.Buffer
		if err := run(context.Background(), []string{"--json"}, &out); err != nil {
			t.Fatalf("%s --json: %v", name, err)
		}
		if !strings.Contains(out.String(), `"ok"`) {
			t.Fatalf("%s --json did not emit the server payload:\n%s", name, out.String())
		}
	}
}

// --- #237: log flags after the run-id ---

func TestLogAcceptsFlagsAfterTheRunID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"run_id":"r","burn":"ci","step":"test","output":"ok\n","total_lines":1}`))
	}))
	defer server.Close()
	configure(t, server)

	// The documented form is `oberth log <run-id> --burn ci --step test`.
	// Before the fix, flags after the positional were a parse error.
	var out bytes.Buffer
	err := runRemoteLog(context.Background(), []string{"run-abc", "--burn", "ci", "--step", "test"}, &out)
	if err != nil {
		t.Fatalf("flags after the run-id failed: %v", err)
	}
	if !strings.Contains(out.String(), "ok") {
		t.Fatalf("output missing content:\n%s", out.String())
	}
}

// --- #237: --json preserves int64 precision ---

func TestJSONPreservesInt64Precision(t *testing.T) {
	// RepoID is int64; decoding through any turns it into float64, which
	// loses precision above 2^53. json.Indent on the raw bytes preserves
	// the original representation.
	payload := `[{"ID":"run-x","RepoID":9007199254740993,"SHA":"abc","Status":"ok"}]`
	configure(t, remoteServer(t, payload))
	var out bytes.Buffer
	if err := runRuns(context.Background(), []string{"--json"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "9007199254740993") {
		t.Fatalf("int64 precision lost in --json output:\n%s", out.String())
	}
}

// --- #237: binary artifact download ---

func TestArtifactDownloadWritesRawBytes(t *testing.T) {
	binary := "\x89PNG\r\n\x1a\nfake-png-body"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/artifacts/img.png") {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte(binary))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"run_id":"r","artifacts":[]}`))
	}))
	defer server.Close()
	configure(t, server)

	var out bytes.Buffer
	if err := runArtifacts(context.Background(), []string{"run-abc", "img.png"}, &out); err != nil {
		t.Fatalf("binary artifact download failed: %v", err)
	}
	if out.String() != binary {
		t.Fatalf("artifact body = %q, want %q", out.String(), binary)
	}
}

// --- #242: status and issues --json is wired through ---

func TestStatusRendersKeyValueWithoutJSON(t *testing.T) {
	payload := `{"database":"ready","vcs":"ready","cluster":"ready","audit":"ready","version":"0.12.15","upstreams":2,"repositories":5}`
	configure(t, remoteServer(t, payload))
	var out bytes.Buffer
	if err := runRemoteStatus(context.Background(), nil, &out); err != nil {
		t.Fatal(err)
	}
	body := out.String()
	for _, want := range []string{"database:", "ready", "0.12.15"} {
		if !strings.Contains(body, want) {
			t.Fatalf("output missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `"database"`) {
		t.Fatalf("status without --json emitted JSON instead of a rendered table:\n%s", body)
	}
}

func TestStatusJSONEmitsRawPayload(t *testing.T) {
	payload := `{"database":"ready","vcs":"ready"}`
	configure(t, remoteServer(t, payload))
	var out bytes.Buffer
	if err := runRemoteStatus(context.Background(), []string{"--json"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"database"`) {
		t.Fatalf("--json did not emit the raw server payload:\n%s", out.String())
	}
}

func TestIssuesRendersTableWithoutJSON(t *testing.T) {
	payload := `{"Issues":[{"ID":42,"State":"open","Kind":"ci","Title":"lint failed"}],"NextBefore":0}`
	configure(t, remoteServer(t, payload))
	var out bytes.Buffer
	if err := runIssues(context.Background(), nil, &out); err != nil {
		t.Fatal(err)
	}
	body := out.String()
	for _, want := range []string{"42", "open", "ci", "lint failed", "ID"} {
		if !strings.Contains(body, want) {
			t.Fatalf("output missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `"Issues"`) {
		t.Fatalf("issues without --json emitted JSON instead of a rendered table:\n%s", body)
	}
}

func TestIssuesEmptyShowsNoIssues(t *testing.T) {
	configure(t, remoteServer(t, `{"Issues":[]}`))
	var out bytes.Buffer
	if err := runIssues(context.Background(), nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no issues") {
		t.Fatalf("empty issue list did not show a message:\n%s", out.String())
	}
}

// --- #445: positional argument rejection ---

func TestStatusRejectsPositionalArgs(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runRemoteStatus(context.Background(), []string{"main"}, &out)
	if err == nil || !strings.Contains(err.Error(), "takes no arguments") {
		t.Fatalf("error = %v, want positional rejection", err)
	}
}

func TestReposRejectsPositionalArgs(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runRepos(context.Background(), []string{"myrepo"}, &out)
	if err == nil || !strings.Contains(err.Error(), "takes no arguments") {
		t.Fatalf("error = %v, want positional rejection", err)
	}
}

func TestIssuesRejectsPositionalArgs(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runIssues(context.Background(), []string{"42"}, &out)
	if err == nil || !strings.Contains(err.Error(), "takes no arguments") {
		t.Fatalf("error = %v, want positional rejection", err)
	}
}

func TestRunsRejectsPositionalArgs(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runRuns(context.Background(), []string{"main"}, &out)
	if err == nil || !strings.Contains(err.Error(), "takes no positional") {
		t.Fatalf("error = %v, want positional rejection", err)
	}
}

// --- #445: short run ID resolution ---

func TestLogResolvesShortRunID(t *testing.T) {
	var requestedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/runs" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"ID":"abcdef1234567890abcdef1234567890","Ref":"main","SHA":"0123456789abcdef","Status":"ok"}]`))
			return
		}
		requestedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"run_id":"abcdef1234567890abcdef1234567890","burn":"ci","step":"test","output":"ok\n","total_lines":1}`))
	}))
	defer server.Close()
	configure(t, server)

	var out bytes.Buffer
	err := runRemoteLog(context.Background(), []string{"--burn", "ci", "--step", "test", "abcdef12"}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(requestedPath, "abcdef1234567890abcdef1234567890") {
		t.Fatalf("short ID was not resolved; requested path = %q", requestedPath)
	}
}

func TestArtifactsResolvesShortRunID(t *testing.T) {
	var requestedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/runs" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"ID":"abcdef1234567890abcdef1234567890","Ref":"main","SHA":"0123456789abcdef","Status":"ok"}]`))
			return
		}
		requestedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"run_id":"abcdef1234567890abcdef1234567890","artifacts":[]}`))
	}))
	defer server.Close()
	configure(t, server)

	var out bytes.Buffer
	err := runArtifacts(context.Background(), []string{"abcdef12"}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(requestedPath, "abcdef1234567890abcdef1234567890") {
		t.Fatalf("short ID was not resolved; requested path = %q", requestedPath)
	}
}
