package installer

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// fakeClaude stands in for the claude CLI's plugin commands: list, install,
// enable and configure keep their state in memory, so a second setup sees
// what the first one did. Option values arrive on standard input only.
type fakeClaude struct {
	t         *testing.T
	ran       [][]string
	installed bool
	enabled   bool
	options   map[string]string
}

func (fake *fakeClaude) deps() Deps {
	return testAdminDeps(Deps{
		LookPath:   func(name string) (string, error) { return "/usr/local/bin/" + name, nil },
		RunCommand: fake.run,
	})
}

func (fake *fakeClaude) run(_ context.Context, input []byte, name string, args ...string) ([]byte, error) {
	fake.ran = append(fake.ran, append([]string{name}, args...))
	line := strings.Join(args, " ")
	if name != "claude" {
		return nil, fmt.Errorf("unexpected command %s %s", name, line)
	}
	switch {
	case line == "plugin list --json":
		if !fake.installed {
			return []byte("[]"), nil
		}
		return json.Marshal([]map[string]any{{"id": ClaudePluginID, "enabled": fake.enabled, "scope": "user"}})
	case strings.HasPrefix(line, "plugin install "+ClaudePluginName+" --marketplace "):
		fake.installed, fake.enabled = true, true
		return []byte("installed"), nil
	case line == "plugin enable "+ClaudePluginID:
		fake.enabled = true
		return nil, nil
	case line == "plugin configure "+ClaudePluginID+" --json":
		inputs := map[string]string{"url": fake.options["url"], "ca_pin": fake.options["ca_pin"], "token": ""}
		configured := []string{}
		for _, key := range []string{"url", "token", "ca_pin"} {
			if fake.options[key] != "" {
				configured = append(configured, key)
			}
		}
		return json.Marshal(map[string]any{"pluginId": ClaudePluginID, "inputs": inputs, "configured": configured})
	case line == "plugin configure "+ClaudePluginID+" --values-stdin":
		var values map[string]string
		if err := json.Unmarshal(input, &values); err != nil {
			return nil, err
		}
		if fake.options == nil {
			fake.options = map[string]string{}
		}
		for key, value := range values {
			fake.options[key] = value
		}
		return []byte("Configuration saved."), nil
	}
	return nil, fmt.Errorf("unexpected command %s %s", name, line)
}

func (fake *fakeClaude) commands() []string {
	lines := make([]string, len(fake.ran))
	for index, command := range fake.ran {
		lines[index] = strings.Join(command, " ")
	}
	return lines
}

type claudeCodeFixture struct {
	home     string
	settings string
	store    string
	caFile   string
	pin      string
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
	authority := testCAPEM(t)
	caFile := filepath.Join(root, "ca.crt")
	if err := os.WriteFile(caFile, authority, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "token"), []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return claudeCodeFixture{
		home:     home,
		settings: filepath.Join(home, ".claude", "settings.json"),
		store:    filepath.Join(home, ".claude", "oberth"),
		caFile:   caFile,
		pin:      testPin(t, authority),
		claude:   &fakeClaude{t: t},
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
		URL:     "https://oberth.example:30443/mcp",
		CA:      fixture.caFile,
		CATrust: CATrustSettings,
		Token:   func(context.Context) (string, error) { return testToken, nil },
		DryRun:  dryRun,
	}
}

// trustedBundle reads env.NODE_EXTRA_CA_CERTS of the settings file.
func (fixture claudeCodeFixture) trustedBundle(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(fixture.settings)
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
	return settings.Env["NODE_EXTRA_CA_CERTS"]
}

// testAuthority is a throwaway CA that can issue a localhost leaf.
type testAuthority struct {
	certificate *x509.Certificate
	key         *ecdsa.PrivateKey
	pem         []byte
}

func newTestAuthority(t *testing.T, name string) testAuthority {
	t.Helper()
	return newTestAuthorityFor(t, name)
}

// newTestAuthorityFor makes a CA; with names it carries critical name
// constraints that permit those DNS names, as the Oberth server CA does.
func newTestAuthorityFor(t *testing.T, name string, permitted ...string) testAuthority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapPrivateKey(t, key)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	if len(permitted) > 0 {
		template.PermittedDNSDomainsCritical = true
		template.PermittedDNSDomains = permitted
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return testAuthority{certificate: certificate, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns a leaf for localhost and 127.0.0.1 and its key.
func (authority testAuthority) issue(t *testing.T) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapPrivateKey(t, key)
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, authority.certificate, &key.PublicKey, authority.key)
	if err != nil {
		t.Fatal(err)
	}
	return der, key
}

func testCAPEM(t *testing.T) []byte {
	t.Helper()
	return newTestAuthority(t, "Oberth test CA").pem
}

func testPin(t *testing.T, authority []byte) string {
	t.Helper()
	block, _ := pem.Decode(authority)
	if block == nil {
		t.Fatal("no PEM certificate")
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:])
}

// The mcp.json record `oberth install` writes for other MCP clients: HTTP, a
// headersHelper instead of a header, and the timeout that lets a 600 s
// run_wait finish.
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

// The pin is the SHA-256 of the CA certificate's DER, in lowercase hex.
func TestCAPinIsTheSHA256OfTheCertificate(t *testing.T) {
	fixture := newClaudeCodeFixture(t)
	for _, authority := range []string{fixture.caFile, string(mustRead(t, fixture.caFile))} {
		pin, err := CAPin(authority)
		if err != nil || pin != fixture.pin || len(pin) != 64 || strings.ToLower(pin) != pin {
			t.Fatalf("CAPin = %q %v, want %s", pin, err, fixture.pin)
		}
	}
	if _, err := CAPin(filepath.Join(fixture.home, "missing.crt")); err == nil {
		t.Fatal("a missing CA file has a pin")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// A clean machine: the CA is stored under Claude Code's directory, the
// settings are merged with every other key kept in order, the plugin is
// installed and configured, and no user-scope MCP server is registered.
func TestClaudeCodeSetupTrustsTheCAAndInstallsThePlugin(t *testing.T) {
	fixture := newClaudeCodeFixture(t)
	fixture.writeSettings(t, `{
  "model": "opus",
  "env": {"FOO": "bar"},
  "permissions": {"allow": ["Bash(ls:*)"]}
}
`)
	changes, err := SetupClaudeCode(t.Context(), fixture.claude.deps(), fixture.setup(false))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 3 || changes[0].Action != "set" || changes[1].Action != "installed" {
		t.Fatalf("changes = %+v", changes)
	}
	stored := filepath.Join(fixture.store, "ca.crt")
	if got := fixture.trustedBundle(t); got != stored {
		t.Fatalf("NODE_EXTRA_CA_CERTS = %q, want %s", got, stored)
	}
	if !bytes.Equal(mustRead(t, stored), mustRead(t, fixture.caFile)) {
		t.Fatal("the stored CA differs from the deployment's")
	}
	if got := strings.TrimSpace(string(mustRead(t, filepath.Join(fixture.store, "ca.sha256")))); got != fixture.pin {
		t.Fatalf("stored pin = %q", got)
	}
	settings := string(mustRead(t, fixture.settings))
	if !inOrder(settings, []string{`"model": "opus"`, `"env"`, `"FOO": "bar"`, `"NODE_EXTRA_CA_CERTS"`, `"permissions"`, `"Bash(ls:*)"`}) {
		t.Fatalf("settings were not merged in place:\n%s", settings)
	}
	if info, err := os.Stat(fixture.settings); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("settings mode = %v %v", info, err)
	}
	want := []string{
		"claude plugin list --json",
		"claude plugin install oberth --marketplace " + DefaultClaudePluginMarketplace,
		"claude plugin configure " + ClaudePluginID + " --json",
		"claude plugin configure " + ClaudePluginID + " --values-stdin",
	}
	if got := fixture.claude.commands(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("claude commands:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	options := fixture.claude.options
	if options["url"] != "https://oberth.example:30443/mcp" || options["ca_pin"] != fixture.pin || options["token"] != "" {
		t.Fatalf("plugin options url=%q ca_pin=%q token set=%v", options["url"], options["ca_pin"], options["token"] != "")
	}
	for _, command := range fixture.claude.commands() {
		if strings.Contains(command, " mcp ") {
			t.Errorf("setup registered an MCP server: %s", command)
		}
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
	settings := mustRead(t, fixture.settings)
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
	want := "claude plugin list --json\nclaude plugin configure " + ClaudePluginID + " --json"
	if got := strings.Join(fixture.claude.commands(), "\n"); got != want {
		t.Errorf("second run ran:\n%s", got)
	}
	if !bytes.Equal(settings, mustRead(t, fixture.settings)) {
		t.Error("second run rewrote the settings")
	}
}

// A token the plugin already holds is kept unless one is given explicitly,
// and a disabled plugin is enabled.
func TestClaudeCodeSetupKeepsTheTokenAndEnablesThePlugin(t *testing.T) {
	fixture := newClaudeCodeFixture(t)
	fixture.claude.installed = true
	setup := fixture.setup(false)
	if _, err := SetupClaudeCode(t.Context(), fixture.claude.deps(), setup); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(fixture.store, "bearer-token")
	before := mustRead(t, tokenPath)
	setup.Token = func(context.Context) (string, error) { t.Fatal("existing token was reread"); return "", nil }
	if _, err := SetupClaudeCode(t.Context(), fixture.claude.deps(), setup); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, mustRead(t, tokenPath)) {
		t.Fatal("saved credential changed")
	}
	if !fixture.claude.enabled {
		t.Fatal("plugin not enabled")
	}
	setup = fixture.setup(false)
	setup.ReplaceToken = true
	if _, err := SetupClaudeCode(t.Context(), fixture.claude.deps(), setup); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(tokenPath); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("bearer file permissions", err)
	}
}

func TestClaudeCodeSetupDryRunChangesNothing(t *testing.T) {
	fixture := newClaudeCodeFixture(t)
	fixture.writeSettings(t, "{\"model\": \"opus\"}\n")
	before := mustRead(t, fixture.settings)
	changes, err := SetupClaudeCode(t.Context(), fixture.claude.deps(), fixture.setup(true))
	if err != nil {
		t.Fatal(err)
	}
	if len(fixture.claude.ran) != 0 {
		t.Errorf("dry run ran %v", fixture.claude.ran)
	}
	if !bytes.Equal(before, mustRead(t, fixture.settings)) {
		t.Error("dry run changed the settings")
	}
	if _, err := os.Stat(fixture.store); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("dry run created %s: %v", fixture.store, err)
	}
	var text strings.Builder
	for _, change := range changes {
		text.WriteString(change.String())
	}
	for _, want := range []string{"would be set", "NODE_EXTRA_CA_CERTS: unset -> " + filepath.Join(fixture.store, "ca.crt"), "would be installed or configured", "url: https://oberth.example:30443/mcp", "ca_pin: " + fixture.pin} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("dry run lacks %q:\n%s", want, &text)
		}
	}
}

// The token reaches Claude Code on standard input only: it is in no argument
// list, no output line and no file this setup writes.
func TestClaudeCodeSetupNeverExposesTheToken(t *testing.T) {
	fixture := newClaudeCodeFixture(t)
	changes, err := SetupClaudeCode(t.Context(), fixture.claude.deps(), fixture.setup(false))
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		if strings.Contains(change.String(), testToken) {
			t.Fatalf("output holds the token:\n%s", change.String())
		}
	}
	for _, command := range fixture.claude.commands() {
		if strings.Contains(command, testToken) {
			t.Fatalf("an argument list holds the token: %s", strings.ReplaceAll(command, testToken, "<token>"))
		}
	}
	saved := filepath.Join(fixture.home, ".config", "oberth", "token")
	err = filepath.WalkDir(fixture.home, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || path == saved || path == filepath.Join(fixture.store, "bearer-token") {
			return err
		}
		if bytes.Contains(mustRead(t, path), []byte(testToken)) {
			t.Errorf("%s holds the token", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// NODE_EXTRA_CA_CERTS names one file. A bundle that already holds the CA is
// kept; one that does not is copied, with the CA added, to oberth/bundle.pem.
func TestClaudeCodeSetupKeepsAnExistingCABundle(t *testing.T) {
	fixture := newClaudeCodeFixture(t)
	other := testCAPEM(t)
	bundle := filepath.Join(fixture.home, "corporate.pem")
	if err := os.WriteFile(bundle, append(append([]byte{}, other...), mustRead(t, fixture.caFile)...), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.writeSettings(t, fmt.Sprintf(`{"env": {"NODE_EXTRA_CA_CERTS": %q}}`, bundle))
	before := mustRead(t, fixture.settings)
	changes, err := SetupClaudeCode(t.Context(), fixture.claude.deps(), fixture.setup(false))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, mustRead(t, fixture.settings)) || !strings.Contains(changes[0].String(), "already holds the Oberth CA") {
		t.Fatalf("a bundle holding the CA was not kept: %+v", changes[0])
	}

	// The bundle does not hold the CA: a copy with it is made and named.
	if err := os.WriteFile(bundle, other, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SetupClaudeCode(t.Context(), fixture.claude.deps(), fixture.setup(false)); err != nil {
		t.Fatal(err)
	}
	merged := filepath.Join(fixture.store, "bundle.pem")
	if got := fixture.trustedBundle(t); got != merged {
		t.Fatalf("NODE_EXTRA_CA_CERTS = %q, want %s", got, merged)
	}
	held := parsePEMCertificates(mustRead(t, merged))
	if len(held) != 2 || !containsAll(held, parsePEMCertificates(other)) || !containsAll(held, parsePEMCertificates(mustRead(t, fixture.caFile))) {
		t.Fatalf("bundle.pem holds %d certificates, want the existing one and the Oberth CA", len(held))
	}
	if got := strings.TrimSpace(string(mustRead(t, filepath.Join(fixture.store, "bundle.source")))); got != bundle {
		t.Fatalf("bundle.source = %q", got)
	}
	if !bytes.Equal(other, mustRead(t, bundle)) {
		t.Fatal("the operator's bundle was changed")
	}
	// Again: nothing changes, and the copy follows its origin.
	changes, err = SetupClaudeCode(t.Context(), fixture.claude.deps(), fixture.setup(false))
	if err != nil || changes[0].Changed() {
		t.Fatalf("second run = %+v %v", changes[0], err)
	}
	third := testCAPEM(t)
	if err := os.WriteFile(bundle, append(append([]byte{}, other...), third...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SetupClaudeCode(t.Context(), fixture.claude.deps(), fixture.setup(false)); err != nil {
		t.Fatal(err)
	}
	if held := parsePEMCertificates(mustRead(t, merged)); len(held) != 3 {
		t.Fatalf("the rebuilt bundle holds %d certificates, want 3", len(held))
	}

	// A named bundle that cannot be read is not replaced.
	fixture.writeSettings(t, `{"env": {"NODE_EXTRA_CA_CERTS": "/nonexistent/bundle.pem"}}`)
	before = mustRead(t, fixture.settings)
	if _, err := SetupClaudeCode(t.Context(), fixture.claude.deps(), fixture.setup(false)); err == nil || !strings.Contains(err.Error(), "cannot be read") {
		t.Fatalf("unreadable bundle: %v", err)
	}
	if !bytes.Equal(before, mustRead(t, fixture.settings)) {
		t.Fatal("settings naming an unreadable bundle were changed")
	}
}

// `--ca-trust none` is a server with a publicly trusted certificate: no CA
// is stored and the plugin gets no pin.
func TestClaudeCodeSetupWithoutACA(t *testing.T) {
	fixture := newClaudeCodeFixture(t)
	setup := fixture.setup(false)
	setup.CA, setup.CATrust = "", CATrustNone
	if _, err := SetupClaudeCode(t.Context(), fixture.claude.deps(), setup); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fixture.settings); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("settings were written: %v", err)
	}
	if _, pinned := fixture.claude.options["ca_pin"]; pinned {
		t.Fatal("a pin was configured without a CA")
	}
	setup.CATrust = CATrustSettings
	if _, err := SetupClaudeCode(t.Context(), fixture.claude.deps(), setup); err == nil || !strings.Contains(err.Error(), "--ca-file") {
		t.Fatalf("settings trust without a CA: %v", err)
	}
}

func TestResolveClaudeCodeSetupUsesTheCLIConfiguration(t *testing.T) {
	fixture := newClaudeCodeFixture(t)
	root := filepath.Join(fixture.home, ".config", "oberth")
	if err := os.WriteFile(filepath.Join(root, "env"), []byte("export OBERTH_BASE_URL=\"https://oberth.example:30443\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	setup, notes, err := ResolveClaudeCodeSetup(ClientSetupFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if setup.URL != "https://oberth.example:30443/mcp" || setup.CA != fixture.caFile || setup.CATrust != CATrustSettings || len(notes) != 0 || setup.ReplaceToken {
		t.Fatalf("setup = %+v notes = %v", setup, notes)
	}
	if token, err := setup.Token(t.Context()); err != nil || token != testToken {
		t.Fatalf("the saved token was not resolved: %v", err)
	}

	// Another server gets neither the saved token nor the saved CA.
	setup, notes, err = ResolveClaudeCodeSetup(ClientSetupFlags{URL: "https://other.example:30443", CATrust: CATrustNone})
	if err != nil {
		t.Fatal(err)
	}
	if setup.URL != "https://other.example:30443/mcp" || setup.Token != nil || setup.CA != "" || len(notes) != 1 {
		t.Fatalf("another server: setup = %+v notes = %v", setup, notes)
	}

	if _, _, err := ResolveClaudeCodeSetup(ClientSetupFlags{CATrust: "system"}); err == nil {
		t.Fatal("--ca-trust system was accepted")
	}
	if err := os.Remove(filepath.Join(root, "env")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ResolveClaudeCodeSetup(ClientSetupFlags{}); err == nil {
		t.Fatal("no server address was accepted")
	}
}

func TestResolveClaudeCodeSetupRefusesAReadableTokenFile(t *testing.T) {
	fixture := newClaudeCodeFixture(t)
	token := filepath.Join(fixture.home, "token")
	if err := os.WriteFile(token, []byte(testToken), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ResolveClaudeCodeSetup(ClientSetupFlags{URL: "https://oberth.example:30443", TokenFile: token}); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("a token file others can read: %v", err)
	}
	if err := os.Chmod(token, 0o600); err != nil {
		t.Fatal(err)
	}
	setup, _, err := ResolveClaudeCodeSetup(ClientSetupFlags{URL: "https://oberth.example:30443", TokenFile: token})
	if err != nil || !setup.ReplaceToken || setup.Token == nil {
		t.Fatalf("an owner-only token file: %+v %v", setup, err)
	}
}

// Client files someone else can change give the plugin neither their
// address nor their token.
func TestResolveClaudeCodeSetupRefusesClientFilesOthersCanChange(t *testing.T) {
	fixture := newClaudeCodeFixture(t)
	root := filepath.Join(fixture.home, ".config", "oberth")
	env := filepath.Join(root, "env")
	if err := os.WriteFile(env, []byte("export OBERTH_BASE_URL=\"https://oberth.example:30443\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(env, 0o620); err != nil {
		t.Fatal(err)
	}
	if setup, _, err := ResolveClaudeCodeSetup(ClientSetupFlags{}); err == nil || !strings.Contains(err.Error(), "not trusted") {
		t.Fatalf("group-writable env = %+v %v", setup, err)
	}
	// An explicit address works without the files, and without their token.
	setup, notes, err := ResolveClaudeCodeSetup(ClientSetupFlags{URL: "https://oberth.example:30443", CAFile: fixture.caFile})
	if err != nil || setup.Token != nil || len(notes) != 1 {
		t.Fatalf("explicit flags = %+v %v %v", setup, notes, err)
	}
}

// `oberth install` configures Claude Code through the same path: CA trust
// and the plugin, with no user-scope MCP server.
func TestInstallConfiguresClaudeCodeThroughClientSetup(t *testing.T) {
	deps, out, home := clientAccessDeps(t, "m\n")
	claude := &fakeClaude{t: t}
	authority := testCAPEM(t)
	deps.KubeClient = fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "oberth-tls", Namespace: "oberth"},
		Data:       map[string][]byte{"tls.crt": authority},
	})
	deps.LookPath = claude.deps().LookPath
	deps.RunCommand = claude.run
	if err := offerClientAccess(t.Context(), Config{}, deps, newTableWriter(deps.Output, false), testToken); err != nil {
		t.Fatal(err)
	}
	if !claude.installed || claude.options["ca_pin"] != testPin(t, authority) || !strings.HasSuffix(claude.options["url"], ":30443/mcp") {
		t.Fatalf("plugin installed=%v options url=%q ca_pin=%q\n%s", claude.installed, claude.options["url"], claude.options["ca_pin"], out)
	}
	for _, command := range claude.commands() {
		if strings.Contains(command, " mcp ") || strings.Contains(command, testToken) {
			t.Errorf("install ran %s", strings.ReplaceAll(command, testToken, "<token>"))
		}
	}
	settings, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil || !strings.Contains(string(settings), filepath.Join(home, ".claude", "oberth", "ca.crt")) {
		t.Fatalf("install did not trust the CA in Claude Code settings: %s %v", settings, err)
	}
}
