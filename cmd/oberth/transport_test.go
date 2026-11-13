package main

import (
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// transportFixture is one TLS server answering both the REST reads and MCP
// tool calls, with its CA and a token in files, so a test can reach it only
// through the transport flags or the client files.
type transportFixture struct {
	server    *httptest.Server
	config    string
	caFile    string
	tokenFile string
	mu        sync.Mutex
	calls     map[string]json.RawMessage
	results   map[string]string
}

const transportToken = "fixture-transport-token"

func newTransportFixture(t *testing.T) *transportFixture {
	t.Helper()
	for _, name := range []string{"OBERTH_BASE_URL", "OBERTH_MCP_URL", "OBERTH_TOKEN", "OBERTH_TOKEN_COMMAND", "OBERTH_CA_CERT"} {
		t.Setenv(name, "")
	}
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	fixture := &transportFixture{config: config, calls: map[string]json.RawMessage{}, results: map[string]string{}}
	fixture.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+transportToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/status":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"vcs":"ok","cluster":"ok","database":"ok","version":"test"}`))
		case "/mcp":
			var request struct {
				ID     int             `json:"id"`
				Params toolsCallParams `json:"params"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				return
			}
			fixture.mu.Lock()
			fixture.calls[request.Params.Name] = request.Params.Arguments
			text := fixture.results[request.Params.Name]
			fixture.mu.Unlock()
			if text == "" {
				text = "{}"
			}
			testWriteJSONRPCResult(w, &request.ID, toolsCallResult{Content: []mcpContent{{Type: "text", Text: text}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fixture.server.Close)
	directory := t.TempDir()
	fixture.caFile = filepath.Join(directory, "ca.crt")
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fixture.server.Certificate().Raw})
	if err := os.WriteFile(fixture.caFile, certificate, 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.tokenFile = filepath.Join(directory, "token")
	if err := os.WriteFile(fixture.tokenFile, []byte(transportToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (fixture *transportFixture) flags() []string {
	return []string{"--url", fixture.server.URL + "/mcp", "--token-file", fixture.tokenFile, "--ca-file", fixture.caFile}
}

func (fixture *transportFixture) arguments(t *testing.T, tool string) map[string]any {
	t.Helper()
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	var arguments map[string]any
	if err := json.Unmarshal(fixture.calls[tool], &arguments); err != nil {
		t.Fatalf("%s arguments %s: %v", tool, fixture.calls[tool], err)
	}
	return arguments
}

// REST and MCP commands accept the same --url/--token-file/--ca-file, and
// --url may name the /mcp endpoint for either (review P1-11).
func TestRESTAndMCPCommandsShareTheTransportFlags(t *testing.T) {
	fixture := newTransportFixture(t)
	var out strings.Builder
	if err := runCLI(t.Context(), append([]string{"status", "--json"}, fixture.flags()...), strings.NewReader(""), &out); err != nil || !strings.Contains(out.String(), `"vcs": "ok"`) {
		t.Fatalf("status over REST with flags = %v %s", err, &out)
	}
	out.Reset()
	if err := runCLI(t.Context(), append([]string{"mcp", "call", "system_status"}, fixture.flags()...), strings.NewReader(""), &out); err != nil {
		t.Fatalf("mcp call with the same flags = %v %s", err, &out)
	}
	for _, command := range [][]string{{"runs"}, {"run", "0123456789abcdef"}, {"repos"}, {"issues"}, {"log", "0123456789abcdef"}, {"artifacts", "0123456789abcdef"}} {
		var help strings.Builder
		if err := runCLI(t.Context(), append(append([]string{}, command...), "--help"), strings.NewReader(""), &help); err != nil {
			t.Fatalf("%v --help: %v", command, err)
		}
		for _, flag := range []string{"-url", "-token-file", "-ca-file"} {
			if !strings.Contains(help.String(), flag) {
				t.Errorf("%v does not accept %s:\n%s", command, flag, &help)
			}
		}
	}
}

// With nothing set, the REST commands read the files oberth install writes,
// including the owner-only token file `oberth client setup` points at.
func TestRESTCommandsReadTheClientFiles(t *testing.T) {
	fixture := newTransportFixture(t)
	var out strings.Builder
	err := runCLI(t.Context(), []string{"status"}, strings.NewReader(""), &out)
	if err == nil || !strings.Contains(err.Error(), "~/.config/oberth") || !strings.Contains(err.Error(), "--url") {
		t.Fatalf("unconfigured status = %v", err)
	}
	directory := filepath.Join(fixture.config, "oberth")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	env := "export OBERTH_BASE_URL=\"" + fixture.server.URL + "\"\nexport OBERTH_CA_CERT=\"" + fixture.caFile + "\"\n"
	if err := os.WriteFile(filepath.Join(directory, "env"), []byte(env), 0o644); err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(fixture.tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "token"), token, 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runCLI(t.Context(), []string{"status", "--json"}, strings.NewReader(""), &out); err != nil || !strings.Contains(out.String(), `"cluster": "ok"`) {
		t.Fatalf("status from the client files = %v %s", err, &out)
	}
	// The environment still wins over the files.
	t.Setenv("OBERTH_TOKEN", "wrong-token")
	if err := runCLI(t.Context(), []string{"status"}, strings.NewReader(""), &out); err == nil {
		t.Fatal("OBERTH_TOKEN did not take precedence over the token file")
	}
}

func TestRunWaitEventsAndSyncStatusVerbs(t *testing.T) {
	fixture := newTransportFixture(t)
	abandon := time.Date(2026, 11, 10, 12, 20, 0, 0, time.UTC).Format(time.RFC3339)
	fixture.results["run_wait"] = `{"state":"held","waited_s":42,"run":{"id":"f0ac7a401717","repo":"github/acme/app","ref":"land/x","sha":"abcdef0123456789","status":"running","display_status":"running","display_reason":"test-service held"},` +
		`"holds":[{"pod":"wf-test-1","step":"test-service","held_since":"2026-11-10T12:00:00Z","abandon_at":"` + abandon + `"}]}`
	fixture.results["events_wait"] = `{"events":[{"id":7,"kind":"run.held","at":"2026-11-10T12:00:00Z","summary":"run f0ac7a40 holds step test-service"}],"next_after_id":7,"latest_id":7,"oldest_id":1,"has_more":false,"waited_s":3}`
	fixture.results["events_since"] = `{"events":[],"next_after_id":9,"latest_id":9,"oldest_id":1,"has_more":false}`
	fixture.results["sync_status"] = `{"observing":true,"locks":[{"lock":"Mutex/oberth-release","holders":[{"run_id":"f0ac7a4017170000","seconds":30}],"waiters":[]}],"history":[]}`

	var out strings.Builder
	if err := runCLI(t.Context(), append([]string{"run", "wait", "f0ac7a40", "--until", "held", "--timeout", "75"}, fixture.flags()...), strings.NewReader(""), &out); err != nil {
		t.Fatalf("run wait = %v", err)
	}
	for _, want := range []string{"state:   held after 42s", "github/acme/app land/x@abcdef0", "held:    step test-service (pod wf-test-1)", "abandoned at " + abandon} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("run wait output lacks %q:\n%s", want, &out)
		}
	}
	if arguments := fixture.arguments(t, "run_wait"); arguments["id"] != "f0ac7a40" || arguments["until"] != "held" || arguments["timeout_s"] != float64(75) {
		t.Fatalf("run_wait arguments = %v", arguments)
	}

	out.Reset()
	if err := runCLI(t.Context(), append([]string{"events", "--wait", "--since", "6", "--kind", "run.held", "--run", "f0ac7a40", "--own"}, fixture.flags()...), strings.NewReader(""), &out); err != nil {
		t.Fatalf("events --wait = %v", err)
	}
	if !strings.Contains(out.String(), "run.held") || !strings.Contains(out.String(), "next: oberth events --since 7") {
		t.Fatalf("events --wait output:\n%s", &out)
	}
	if arguments := fixture.arguments(t, "events_wait"); arguments["after_id"] != float64(6) || arguments["own"] != true ||
		arguments["run_ids"].([]any)[0] != "f0ac7a40" || arguments["kinds"].([]any)[0] != "run.held" {
		t.Fatalf("events_wait arguments = %v", arguments)
	}

	out.Reset()
	if err := runCLI(t.Context(), append([]string{"events"}, fixture.flags()...), strings.NewReader(""), &out); err != nil || !strings.Contains(out.String(), "no events") || !strings.Contains(out.String(), "--since 9") {
		t.Fatalf("events = %v\n%s", err, &out)
	}
	if arguments := fixture.arguments(t, "events_since"); arguments["after_id"] != float64(0) {
		t.Fatalf("events_since arguments = %v", arguments)
	}
	if err := runCLI(t.Context(), append([]string{"events", "--run", "f0ac7a40"}, fixture.flags()...), strings.NewReader(""), &out); err == nil {
		t.Fatal("--run without --wait accepted")
	}

	out.Reset()
	if err := runCLI(t.Context(), append([]string{"sync", "status", "--lock", "Mutex/oberth-release"}, fixture.flags()...), strings.NewReader(""), &out); err != nil ||
		!strings.Contains(out.String(), "Mutex/oberth-release") || !strings.Contains(out.String(), "f0ac7a401717 (30s)") {
		t.Fatalf("sync status = %v\n%s", err, &out)
	}
	out.Reset()
	if err := runCLI(t.Context(), append([]string{"sync", "status", "--json"}, fixture.flags()...), strings.NewReader(""), &out); err != nil || !strings.HasPrefix(out.String(), `{"observing":true`) {
		t.Fatalf("sync status --json = %v\n%s", err, &out)
	}
}
