package installer

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// fakeClaude stands in for the claude CLI: add-json and remove edit the user
// configuration file the way Claude Code does, so a second setup sees what
// the first one registered.
type fakeClaude struct {
	t       *testing.T
	config  string
	ran     [][]string
	failGet bool
}

func (fake *fakeClaude) deps() Deps {
	return Deps{
		LookPath:   func(name string) (string, error) { return "/usr/local/bin/" + name, nil },
		RunCommand: fake.run,
	}
}

func (fake *fakeClaude) run(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
	fake.ran = append(fake.ran, append([]string{name}, args...))
	if name != "claude" || len(args) < 3 || args[0] != "mcp" {
		return nil, fmt.Errorf("unexpected command %s %v", name, args)
	}
	document := map[string]any{}
	if body, err := os.ReadFile(fake.config); err == nil {
		if err := json.Unmarshal(body, &document); err != nil {
			return nil, err
		}
	}
	servers, _ := document["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	switch args[1] {
	case "get":
		if fake.failGet {
			return []byte("oberth:\n  Status: ✗ Failed to connect\n"), nil
		}
		return []byte("oberth:\n  Scope: User config\n  Status: ✔ Connected\n"), nil
	case "add-json":
		if _, exists := servers[args[2]]; exists {
			return []byte("MCP server oberth already exists in user config"), errors.New("exit status 1")
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(args[3]), &entry); err != nil {
			return nil, err
		}
		servers[args[2]] = entry
	case "remove":
		delete(servers, args[2])
	default:
		return nil, fmt.Errorf("unexpected claude mcp %s", args[1])
	}
	document["mcpServers"] = servers
	body, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	return nil, os.WriteFile(fake.config, body, 0o600)
}

func (fake *fakeClaude) writeServer(t *testing.T, entry map[string]any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"numStartups": 3, "mcpServers": map[string]any{"oberth": entry}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fake.config, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

type claudeCodeFixture struct {
	home     string
	settings string
	caFile   string
	token    string
	claude   *fakeClaude
}

func newClaudeCodeFixture(t *testing.T) claudeCodeFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	for _, key := range []string{"OBERTH_MCP_URL", "OBERTH_BASE_URL", "OBERTH_CA_CERT", "OBERTH_TOKEN", "OBERTH_TOKEN_COMMAND"} {
		t.Setenv(key, "")
	}
	root := filepath.Join(home, ".config", "oberth")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(root, "ca.crt")
	if err := os.WriteFile(caFile, testCAPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "token"), []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return claudeCodeFixture{
		home:     home,
		settings: filepath.Join(home, ".claude", "settings.json"),
		caFile:   caFile,
		token:    testToken,
		claude:   &fakeClaude{t: t, config: filepath.Join(home, ".claude.json")},
	}
}

func (fixture claudeCodeFixture) writeSettings(t *testing.T, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(fixture.settings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.settings, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
}

func (fixture claudeCodeFixture) setup(dryRun bool) ClaudeCodeSetup {
	return ClaudeCodeSetup{
		URL:          "https://oberth.example:30443/mcp",
		TokenCommand: tokenFileReadCommand,
		CAFile:       fixture.caFile,
		CATrust:      CATrustSettings,
		DryRun:       dryRun,
	}
}

func testCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Oberth test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// The rendered entry is what both `oberth install` and `oberth client setup`
// register: HTTP, a headersHelper instead of a header, and the timeout that
// lets a 600 s run_wait finish.
func TestRenderMCPConfigCarriesHeadersHelperAndTimeout(t *testing.T) {
	body, err := renderMCPConfig("https://oberth.example:30443/mcp", "pass show oberth/token")
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "mcpServers": {
    "oberth": {
      "type": "http",
      "url": "https://oberth.example:30443/mcp",
      "headersHelper": "printf '{\"Authorization\":\"Bearer %s\"}' \"$(pass show oberth/token)\"",
      "timeout": 630000
    }
  }
}
`
	if string(body) != want {
		t.Fatalf("rendered MCP config:\n%s\nwant:\n%s", body, want)
	}
}

func TestClaudeCodeSetupRegistersAndMergesSettings(t *testing.T) {
	fixture := newClaudeCodeFixture(t)
	original := `{
  "hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "true"}]}]},
  "enabledPlugins": {"telegram@claude-plugins-official": false},
  "model": "opus",
  "env": {"FOO": "bar"}
}
`
	fixture.writeSettings(t, original)

	changes, err := SetupClaudeCode(t.Context(), fixture.claude.deps(), fixture.setup(false))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 || changes[0].Action != "registered" || changes[1].Action != "set" {
		t.Fatalf("changes = %+v", changes)
	}
	if len(fixture.claude.ran) != 1 || strings.Join(fixture.claude.ran[0][:3], " ") != "claude mcp add-json" || fixture.claude.ran[0][len(fixture.claude.ran[0])-1] != "user" {
		t.Fatalf("commands = %v", fixture.claude.ran)
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(fixture.claude.ran[0][4]), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["type"] != "http" || entry["url"] != "https://oberth.example:30443/mcp" || entry["timeout"] != float64(630000) ||
		entry["headersHelper"] != headersHelperFor(tokenFileReadCommand) || entry["headers"] != nil {
		t.Fatalf("registered entry = %v", entry)
	}

	merged, err := os.ReadFile(fixture.settings)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(merged, &document); err != nil {
		t.Fatal(err)
	}
	var originalDocument map[string]any
	if err := json.Unmarshal([]byte(original), &originalDocument); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"hooks", "enabledPlugins", "model"} {
		want, _ := json.Marshal(originalDocument[key])
		got, _ := json.Marshal(document[key])
		if !bytes.Equal(want, got) {
			t.Errorf("%s changed: %s -> %s", key, want, got)
		}
	}
	environment, _ := document["env"].(map[string]any)
	if environment["FOO"] != "bar" || environment["NODE_EXTRA_CA_CERTS"] != fixture.caFile {
		t.Errorf("env = %v", environment)
	}
	// Key order is the operator's: the merged file differs only where it was changed.
	if order := []string{`"hooks"`, `"enabledPlugins"`, `"model"`, `"env"`, `"FOO"`, `"NODE_EXTRA_CA_CERTS"`}; !inOrder(string(merged), order) {
		t.Errorf("key order changed:\n%s", merged)
	}
	if info, err := os.Stat(fixture.settings); err != nil || info.Mode().Perm() != 0o640 {
		t.Errorf("settings mode = %v, %v; want the original 0640", info.Mode().Perm(), err)
	}
}

func inOrder(text string, needles []string) bool {
	position := 0
	for _, needle := range needles {
		index := strings.Index(text[position:], needle)
		if index < 0 {
			return false
		}
		position += index + len(needle)
	}
	return true
}

func TestClaudeCodeSetupIsIdempotent(t *testing.T) {
	fixture := newClaudeCodeFixture(t)
	if _, err := SetupClaudeCode(t.Context(), fixture.claude.deps(), fixture.setup(false)); err != nil {
		t.Fatal(err)
	}
	settings, err := os.ReadFile(fixture.settings)
	if err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(fixture.claude.config)
	if err != nil {
		t.Fatal(err)
	}
	fixture.claude.ran = nil

	changes, err := SetupClaudeCode(t.Context(), fixture.claude.deps(), fixture.setup(false))
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		if change.Changed() {
			t.Errorf("second run changed %+v", change)
		}
	}
	if len(fixture.claude.ran) != 0 {
		t.Errorf("second run ran %v", fixture.claude.ran)
	}
	if after, _ := os.ReadFile(fixture.settings); !bytes.Equal(after, settings) {
		t.Errorf("second run rewrote settings")
	}
	if after, _ := os.ReadFile(fixture.claude.config); !bytes.Equal(after, config) {
		t.Errorf("second run rewrote the Claude Code configuration")
	}
}

// A machine configured by hand the same way (its own headersHelper, the
// timeout, the CA in settings) is already done.
func TestClaudeCodeSetupKeepsAnEquivalentHandWrittenEntry(t *testing.T) {
	fixture := newClaudeCodeFixture(t)
	fixture.claude.writeServer(t, map[string]any{
		"type":          "http",
		"url":           "https://oberth.example:30443/mcp",
		"headersHelper": `printf '{"Authorization":"Bearer %s"}' "$(cat $HOME/.config/oberth/token)"`,
		"timeout":       630000,
	})
	fixture.writeSettings(t, fmt.Sprintf(`{"env": {"NODE_EXTRA_CA_CERTS": %q}}`, fixture.caFile))

	changes, err := SetupClaudeCode(t.Context(), fixture.claude.deps(), fixture.setup(false))
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		if change.Changed() {
			t.Errorf("hand-written equivalent changed: %+v", change)
		}
	}
	if len(fixture.claude.ran) != 0 {
		t.Errorf("ran %v", fixture.claude.ran)
	}

	// An explicit token source is the operator asking for that helper.
	setup := fixture.setup(false)
	setup.ExplicitToken = true
	changes, err = SetupClaudeCode(t.Context(), fixture.claude.deps(), setup)
	if err != nil {
		t.Fatal(err)
	}
	if changes[0].Action != "updated" || len(fixture.claude.ran) != 2 || fixture.claude.ran[0][2] != "remove" {
		t.Fatalf("explicit token source: %+v, ran %v", changes, fixture.claude.ran)
	}
}

// An entry from before the headersHelper (a literal or ${VAR} Authorization
// header, no timeout) is replaced, and the change names each difference.
func TestClaudeCodeSetupReplacesAHeaderEntryAndAddsTheTimeout(t *testing.T) {
	fixture := newClaudeCodeFixture(t)
	fixture.claude.writeServer(t, map[string]any{
		"type":    "http",
		"url":     "https://oberth.example:30443/mcp",
		"headers": map[string]any{"Authorization": "Bearer " + testToken},
	})

	changes, err := SetupClaudeCode(t.Context(), fixture.claude.deps(), fixture.setup(false))
	if err != nil {
		t.Fatal(err)
	}
	details := strings.Join(changes[0].Details, "\n")
	for _, want := range []string{"headers: removed a literal bearer token", "headersHelper: unset ->", "timeout: unset (Claude Code default 60000 ms) -> 630000 ms"} {
		if !strings.Contains(details, want) {
			t.Errorf("details lack %q:\n%s", want, details)
		}
	}
	if strings.Contains(details, testToken) {
		t.Error("the change report printed the token")
	}
	body, err := os.ReadFile(fixture.claude.config)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), testToken) {
		t.Error("the literal token survived the update")
	}
}

func TestClaudeCodeSetupDryRunChangesNothing(t *testing.T) {
	fixture := newClaudeCodeFixture(t)
	fixture.writeSettings(t, `{"model": "opus"}`)

	changes, err := SetupClaudeCode(t.Context(), fixture.claude.deps(), fixture.setup(true))
	if err != nil {
		t.Fatal(err)
	}
	if len(fixture.claude.ran) != 0 {
		t.Errorf("dry run ran %v", fixture.claude.ran)
	}
	if _, err := os.Stat(fixture.claude.config); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("dry run registered a server: %v", err)
	}
	if body, _ := os.ReadFile(fixture.settings); string(body) != `{"model": "opus"}` {
		t.Errorf("dry run wrote settings: %s", body)
	}
	if len(changes) != 2 || changes[0].Action != "would be registered" || changes[1].Action != "would be set" {
		t.Fatalf("changes = %+v", changes)
	}
	if !strings.Contains(changes[0].String(), "run: claude mcp add-json oberth '") {
		t.Errorf("dry run does not print the command:\n%s", changes[0])
	}
}

// docs/remote-cli.md: no token is written to disk. Every file under the home
// directory except the token file itself, and every argument passed to the
// claude CLI, is searched for it.
func TestClaudeCodeSetupNeverWritesTheToken(t *testing.T) {
	fixture := newClaudeCodeFixture(t)
	t.Setenv("OBERTH_TOKEN", testToken)
	setup, notes, err := ResolveClaudeCodeSetup(ClientSetupFlags{URL: "https://oberth.example:30443"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "OBERTH_TOKEN is set but not used") {
		t.Errorf("notes = %v", notes)
	}
	changes, err := SetupClaudeCode(t.Context(), fixture.claude.deps(), setup)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		if strings.Contains(change.String(), testToken) {
			t.Errorf("change report holds the token: %s", change)
		}
	}
	for _, command := range fixture.claude.ran {
		if strings.Contains(strings.Join(command, " "), testToken) {
			t.Errorf("claude argument list holds the token: %v", command)
		}
	}
	tokenFile := filepath.Join(fixture.home, ".config", "oberth", "token")
	written := 0
	err = filepath.WalkDir(fixture.home, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || path == tokenFile {
			return walkErr
		}
		written++
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(body), testToken) {
			t.Errorf("%s holds the token", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if written < 3 {
		t.Fatalf("expected the CA, the Claude Code configuration and settings; found %d files", written)
	}
}

// NODE_EXTRA_CA_CERTS names one file. A bundle that already holds the CA is
// kept; one that does not is a conflict the operator resolves, never a
// silent overwrite that would drop their other CA.
func TestClaudeCodeSetupRespectsAnotherCABundle(t *testing.T) {
	fixture := newClaudeCodeFixture(t)
	authority, err := os.ReadFile(fixture.caFile)
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(fixture.home, "bundle.pem")
	if err := os.WriteFile(bundle, append(testCAPEM(t), authority...), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.writeSettings(t, fmt.Sprintf(`{"env": {"NODE_EXTRA_CA_CERTS": %q}}`, bundle))
	changes, err := SetupClaudeCode(t.Context(), fixture.claude.deps(), fixture.setup(false))
	if err != nil || changes[1].Changed() {
		t.Fatalf("bundle holding the CA: %+v %v", changes, err)
	}

	other := filepath.Join(fixture.home, "other.pem")
	if err := os.WriteFile(other, testCAPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.writeSettings(t, fmt.Sprintf(`{"env": {"NODE_EXTRA_CA_CERTS": %q}}`, other))
	if _, err := SetupClaudeCode(t.Context(), fixture.claude.deps(), fixture.setup(false)); err == nil || !strings.Contains(err.Error(), "does not hold the Oberth CA") {
		t.Fatalf("conflicting bundle: %v", err)
	}
	if body, _ := os.ReadFile(fixture.settings); !strings.Contains(string(body), other) {
		t.Error("the operator's bundle was replaced")
	}
}

func TestClaudeCodeSetupSystemTrustPrintsTheCommand(t *testing.T) {
	fixture := newClaudeCodeFixture(t)
	setup := fixture.setup(false)
	setup.CATrust = CATrustSystem
	changes, err := SetupClaudeCode(t.Context(), fixture.claude.deps(), setup)
	if err != nil {
		t.Fatal(err)
	}
	if changes[1].Action != "manual" || !strings.Contains(strings.Join(changes[1].Details, " "), "sudo") {
		t.Fatalf("system trust = %+v", changes[1])
	}
	if _, err := os.Stat(fixture.settings); !errors.Is(err, fs.ErrNotExist) {
		t.Error("system trust wrote Claude Code settings")
	}
}

func TestResolveClaudeCodeSetupUsesTheCLIConfiguration(t *testing.T) {
	fixture := newClaudeCodeFixture(t)
	root := filepath.Join(fixture.home, ".config", "oberth")
	if err := os.WriteFile(filepath.Join(root, "env"), []byte(renderClientEnv("https://oberth.example:30443", fixture.caFile, "pass show oberth/token")), 0o600); err != nil {
		t.Fatal(err)
	}
	setup, _, err := ResolveClaudeCodeSetup(ClientSetupFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if setup.URL != "https://oberth.example:30443/mcp" || setup.CAFile != fixture.caFile || setup.TokenCommand != "pass show oberth/token" || setup.ExplicitToken || setup.CATrust != CATrustSettings {
		t.Fatalf("resolved = %+v", setup)
	}

	setup, _, err = ResolveClaudeCodeSetup(ClientSetupFlags{URL: "https://other.example/mcp", TokenFile: filepath.Join(root, "token")})
	if err != nil {
		t.Fatal(err)
	}
	if setup.URL != "https://other.example/mcp" || !setup.ExplicitToken || setup.TokenCommand != "cat "+shellQuote(filepath.Join(root, "token")) || setup.CAFile != "" {
		t.Fatalf("flags = %+v (the saved CA belongs to the saved server)", setup)
	}
	// The saved token source is never registered for another server.
	for name, configure := range map[string]func() ClientSetupFlags{
		"flag": func() ClientSetupFlags { return ClientSetupFlags{URL: "https://other.example/mcp"} },
		"variable": func() ClientSetupFlags {
			t.Setenv("OBERTH_MCP_URL", "https://other.example/mcp")
			return ClientSetupFlags{}
		},
	} {
		if setup, _, err = ResolveClaudeCodeSetup(configure()); err == nil || !strings.Contains(err.Error(), "never registered for another server") {
			t.Fatalf("%s: other server = %+v %v", name, setup, err)
		}
	}
}

func TestResolveClaudeCodeSetupRefusesAReadableTokenFile(t *testing.T) {
	fixture := newClaudeCodeFixture(t)
	token := filepath.Join(fixture.home, ".config", "oberth", "token")
	if err := os.Chmod(token, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ResolveClaudeCodeSetup(ClientSetupFlags{URL: "https://oberth.example:30443"}); err == nil || !strings.Contains(err.Error(), "expected 0600") {
		t.Fatalf("readable token file: %v", err)
	}
	if _, _, err := ResolveClaudeCodeSetup(ClientSetupFlags{URL: "http://oberth.example"}); err == nil {
		t.Fatal("a cleartext non-loopback URL was accepted")
	}
}

func TestVerifyClaudeCodeReportsAFailedConnection(t *testing.T) {
	fixture := newClaudeCodeFixture(t)
	status, err := VerifyClaudeCode(t.Context(), fixture.claude.deps())
	if err != nil || status != "Status: ✔ Connected" {
		t.Fatalf("connected: %q %v", status, err)
	}
	fixture.claude.failGet = true
	if _, err := VerifyClaudeCode(t.Context(), fixture.claude.deps()); err == nil {
		t.Fatal("a failed connection verified")
	}
}

// `oberth install` configures Claude Code through the same path, timeout and
// CA trust included.
func TestInstallConfiguresClaudeCodeThroughClientSetup(t *testing.T) {
	deps, _, home := clientAccessDeps(t, "m\n")
	claude := &fakeClaude{t: t, config: filepath.Join(home, ".claude.json")}
	deps.KubeClient = fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "oberth-tls", Namespace: "oberth"},
		Data:       map[string][]byte{"tls.crt": testCAPEM(t)},
	})
	deps.LookPath = claude.deps().LookPath
	deps.RunCommand = claude.run
	if err := offerClientAccess(t.Context(), Config{}, deps, newTableWriter(deps.Output, false), testToken); err != nil {
		t.Fatal(err)
	}
	if len(claude.ran) != 1 {
		t.Fatalf("claude commands = %v", claude.ran)
	}
	if !strings.Contains(claude.ran[0][4], `"timeout":630000`) {
		t.Errorf("install registered no timeout: %s", claude.ran[0][4])
	}
	settings, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil || !strings.Contains(string(settings), filepath.Join(home, ".config", "oberth", "ca.crt")) {
		t.Fatalf("install did not trust the CA in Claude Code settings: %s %v", settings, err)
	}
}
