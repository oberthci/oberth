package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestBridgeDisplayBlock(t *testing.T) {
	reason := strings.Repeat("r", 40)
	run := map[string]any{"id": "run1", "repo": "org/app", "ref": "refs/heads/topic", "status": "failed", "display_reason": reason, "detail": "native diagnostic", "steps": []any{map[string]any{"name": "test", "status": "failed", "display_reason": "test failed", "detail": "native step detail"}}, "failure": map[string]any{"failed_step": "test", "retry_hint": map[string]any{"action": "none", "reason_code": "author"}}}
	for _, tool := range []string{"run_get", "run_wait", "status", "wait", "events_wait"} {
		t.Run(tool, func(t *testing.T) { assertToolDisplay(t, tool, run) })
	}
	t.Run("run_list", func(t *testing.T) { assertListDisplay(t, run) })
	if _, ok := Display("repo_list", []byte(`{}`)); ok {
		t.Fatal("unrelated display appended")
	}
	if text, ok := Display("run_wait", []byte(`{"run":null,"timed_out":false}`)); !ok || !strings.Contains(text, "No run matched.") {
		t.Fatal("empty wait lost its display block")
	}
}

func assertToolDisplay(t *testing.T, tool string, run map[string]any) {
	t.Helper()
	value := any(run)
	switch tool {
	case "run_wait", "wait":
		value = map[string]any{"run": run, "timed_out": false}
	case "events_wait":
		value = map[string]any{"events": []any{run}, "next_after_id": 1, "timed_out": false}
	}
	data, _ := json.Marshal(value)
	text, ok := Display(tool, data)
	if !ok || !strings.Contains(text, strings.Repeat("r", 40)) || !strings.Contains(text, "native diagnostic") {
		t.Fatalf("incomplete %s display: %s", tool, text)
	}
	if !strings.Contains(text, "retry hint / action: none") || !strings.Contains(text, "oberth log run1 test") {
		t.Fatalf("missing retry/log instruction: %s", text)
	}
	var cli bytes.Buffer
	if err := (application{out: &cli}).render(data, false, commonOptions{detail: true}); err != nil || text != safeDisplay(cli.String()) {
		t.Fatal("bridge uses a second renderer")
	}
}

// A run list's block is the CLI's table with whole reasons, never a view per row.
func assertListDisplay(t *testing.T, run map[string]any) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"runs": []any{run, run}, "next_cursor": "run1"})
	text, ok := Display("run_list", data)
	var cli bytes.Buffer
	if err := (application{out: &cli}).render(data, false, commonOptions{}); err != nil || !ok || text != safeDisplay(cli.String()) {
		t.Fatalf("list block differs from oberth run list: %s", text)
	}
	if !strings.Contains(text, strings.Repeat("r", 40)) || !strings.Contains(text, "oberth run list --before run1") {
		t.Fatalf("list block lost a reason or the next page: %s", text)
	}
	if strings.Contains(text, "native diagnostic") || strings.Contains(text, "Run run1") || len(strings.Split(strings.TrimSpace(text), "\n")) != 5 {
		t.Fatalf("list block repeats a run view per row: %s", text)
	}
}

func TestDisplayStripsTerminalControls(t *testing.T) {
	for _, text := range []string{"reason\x1b[31mfailed\x1b[0m\u202e", "reason\x1b]title\aafter", "reason\u009b31mfailed\u009b0m"} {
		value := safeDisplay(text)
		if strings.ContainsAny(value, "\x1b\u202e\a\u009b") || strings.Contains(value, "31m") || strings.Contains(value, "title") {
			t.Fatalf("terminal control survived: %q", value)
		}
		data, _ := json.Marshal(map[string]any{"id": "run1", "status": "failed", "ref": "topic", "detail": text})
		formatted, ok := Display("run_get", data)
		if !ok || strings.ContainsAny(formatted, "\x1b\u202e\a\u009b") || strings.Contains(formatted, "31m") || strings.Contains(formatted, "title") {
			t.Fatal("display bypassed control stripping")
		}
	}
}

func TestRegistrationNamesAbsoluteExecutable(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "installed", "oberth")
	if err := os.Mkdir(filepath.Dir(real), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("installed"), 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "oberth-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "oberth"), []byte("planted"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", ".")
	t.Chdir(root)
	var out bytes.Buffer
	a := application{out: &out, directory: root, executable: func() (string, error) { return link, nil }}
	if err := a.agentTools("uplink"); err != nil {
		t.Fatal(err)
	}
	if out.String() != `agent tools: stdio MCP server "oberth" = `+real+" mcp --profile uplink\n" {
		t.Fatalf("registration picked checkout or unresolved link: %s", out.String())
	}
	assertDefaultRegistration(t, a, &out)

	out.Reset()
	s := flowSession{app: a}
	if err := s.joined(model.InviteCertificate{Identity: "agent"}, "profile", filepath.Join(root, "ca.crt"), false); err != nil || !strings.Contains(out.String(), real+" mcp --profile profile") {
		t.Fatal("join omitted registration")
	}
	assertInstallDispatchStaysSilent(t, a, &out)
}

func assertInstallDispatchStaysSilent(t *testing.T, a application, out *bytes.Buffer) {
	t.Helper()
	out.Reset()
	a.install = func(context.Context, installationRequest, io.Reader, io.Writer) error { return nil }
	if code, ok := a.installEntry(t.Context(), []string{"install", "--yes"}); code != 0 || !ok || out.Len() != 0 {
		t.Fatal("install dispatch printed a hint without an apply", out.String())
	}
}

func TestMCPEntryUsesPinnedProfile(t *testing.T) {
	t.Run("explicit", func(t *testing.T) { assertPinnedMCPEntry(t, true) })
	t.Run("checkout", func(t *testing.T) { assertPinnedMCPEntry(t, false) })
}
func assertPinnedMCPEntry(t *testing.T, explicit bool) {
	t.Helper()
	a, diag := pinnedMCPApplication(t)
	args := []string{"mcp"}
	if explicit {
		args = append(args, "--profile", "pinned")
	}
	a.git = func(_ context.Context, words []string) (string, error) {
		if explicit {
			return "", errors.New("explicit profile was overridden by checkout")
		}
		if strings.Join(words, " ") != "config --local --get oberth.uplinkProfile" {
			return "", errors.New("bridge did not read a local checkout binding")
		}
		return "pinned", nil
	}
	encoder, decoder := mcpPeer(t, a, args)
	if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": "init", "method": "initialize", "params": map[string]any{"protocolVersion": "2025-06-18"}}); err != nil {
		t.Fatal(err)
	}
	var reply map[string]json.RawMessage
	if err := decoder.Decode(&reply); err != nil || reply["error"] != nil {
		t.Fatalf("pinned profile overridden: %v %s", err, diag.String())
	}
	if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"}); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&reply); err != nil || !bytes.Contains(reply["result"], []byte(`"run_get"`)) {
		t.Fatal("real tool catalogue not forwarded")
	}
	if diag.Len() != 0 {
		t.Fatal("protocol diagnostics leaked")
	}
}

// mcpPeer runs the application's stdio bridge and returns its protocol ends.
func mcpPeer(t *testing.T, a application, args []string) (*json.Encoder, *json.Decoder) {
	t.Helper()
	in, write := io.Pipe()
	read, out := io.Pipe()
	a.in, a.out = in, out
	done := make(chan int, 1)
	go func() {
		defer close(done)
		defer in.Close()
		defer out.Close()
		done <- a.run(t.Context(), args)
	}()
	t.Cleanup(func() { _ = write.Close(); _ = read.Close(); <-done })
	return json.NewEncoder(write), json.NewDecoder(read)
}

// A host shows the model a reply's structured content alone, so a run read
// reaches it as the JSON text followed by the rendered block, as the server's
// instructions promise.
func TestMCPRunReadEndsWithDisplayBlock(t *testing.T) {
	b := fake.Bindings()
	b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Value: apicontract.RunRecord{RunSummary: stepmodel.RunSummary{
		ID: "run1", Repo: "org/app", Ref: "refs/heads/topic", Status: stepmodel.RunFailed, DisplayReason: "step failed"}}}
	a, diag := boundMCPApplication(t, b)
	encoder, decoder := mcpPeer(t, a, []string{"mcp", "--profile", "pinned"})
	var reply struct {
		Result struct {
			Content []struct{ Type, Text string }
			Tools   []map[string]json.RawMessage
			Other   json.RawMessage `json:"structuredContent"`
		}
		Error json.RawMessage
	}
	for _, request := range []map[string]any{
		{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-06-18"}},
		{"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
		{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{"name": "run_get", "arguments": map[string]any{"id": "run1"}}},
	} {
		if encoder.Encode(request) != nil || decoder.Decode(&reply) != nil || reply.Error != nil {
			t.Fatalf("bridge refused %v: %s", request["method"], diag.String())
		}
		for _, tool := range reply.Result.Tools {
			if tool["outputSchema"] != nil {
				t.Fatal("listed output schema demands structured content")
			}
		}
	}
	content := reply.Result.Content
	if len(content) != 2 || reply.Result.Other != nil {
		t.Fatalf("run read hides its display block: %+v", reply.Result)
	}
	block, ok := Display("run_get", json.RawMessage(content[0].Text))
	if !ok || content[1].Text != "\n"+block || !strings.Contains(block, "step failed") {
		t.Fatalf("display block differs from the CLI's: %q", content[1].Text)
	}
}

func TestMCPHelpAndArguments(t *testing.T) {
	a, out, diag := fixture(t, fake.Bindings())
	for _, args := range [][]string{{"help", "mcp"}, {"mcp", "--help"}} {
		out.Reset()
		if a.run(t.Context(), args) != 0 || !strings.Contains(out.String(), "stdio") {
			t.Fatal("MCP help missing")
		}
	}
	for _, args := range [][]string{{"mcp", "--url", "https://127.0.0.1"}, {"mcp", "other"}} {
		if a.run(t.Context(), args) != 2 {
			t.Fatal("MCP accepted an override", diag.String())
		}
	}
	out.Reset()
	if a.run(t.Context(), []string{"help"}) != 0 || !strings.Contains(out.String(), "mcp:") || strings.Index(out.String(), "mcp:") < strings.Index(out.String(), "Getting started:") {
		t.Fatal("MCP displaced ordinary first-screen help")
	}
}

func pinnedMCPApplication(t *testing.T) (application, *bytes.Buffer) {
	t.Helper()
	return boundMCPApplication(t, fake.Bindings())
}

// boundMCPApplication serves bindings to a joined uplink profile named pinned.
func boundMCPApplication(t *testing.T, bindings apicontract.Bindings) (application, *bytes.Buffer) {
	t.Helper()
	a, _, diag := fixture(t, bindings)
	base := t.TempDir()
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "pinned")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"installation.json": `{"server_url":"https://127.0.0.1","uplink":true}`, "token": "valid"} {
		if err := client.WritePrivate(filepath.Join(dir, name), []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	a.base = base
	a.env = func(name string) string {
		if name == "OBERTH_PROFILE" {
			return "pinned"
		}
		return "https://wrong.invalid"
	}
	return a, diag
}

func assertDefaultRegistration(t *testing.T, a application, out *bytes.Buffer) {
	t.Helper()
	out.Reset()
	a.executable = nil
	actual, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	actual, err = filepath.EvalSymlinks(actual)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.agentTools("uplink"); err != nil || !strings.Contains(out.String(), actual+" mcp") {
		t.Fatal("default registration used PATH or checkout")
	}
}
