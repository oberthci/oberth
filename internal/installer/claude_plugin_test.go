package installer

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func pluginPath(t *testing.T, elements ...string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join(append([]string{"..", "..", "claude-plugin"}, elements...)...))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// The repository is a Claude Code plugin marketplace with one plugin,
// claude-plugin/. Its manifests must parse and name each other; the plugin
// asks for the URL, the token (sensitive) and the CA pin, and its MCP entry
// uses them in a plain url and Authorization header with the 630 s timeout.
func TestClaudePluginManifests(t *testing.T) {
	read := func(path string, into any) {
		t.Helper()
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(body, into); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	var marketplace struct {
		Name    string                `json:"name"`
		Owner   struct{ Name string } `json:"owner"`
		Plugins []struct {
			Name        string `json:"name"`
			Source      string `json:"source"`
			Description string `json:"description"`
		} `json:"plugins"`
	}
	read(filepath.Join("..", "..", ".claude-plugin", "marketplace.json"), &marketplace)
	if marketplace.Name != ClaudePluginMarketplaceName || marketplace.Owner.Name == "" || len(marketplace.Plugins) != 1 {
		t.Fatalf("marketplace = %+v", marketplace)
	}
	entry := marketplace.Plugins[0]
	if entry.Name != ClaudePluginName || entry.Source != "./claude-plugin" || entry.Description == "" {
		t.Fatalf("marketplace plugin = %+v", entry)
	}
	type option struct {
		Type        string `json:"type"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Sensitive   bool   `json:"sensitive"`
		Required    bool   `json:"required"`
	}
	var plugin struct {
		Name        string            `json:"name"`
		Description string            `json:"description"`
		Version     *string           `json:"version"`
		UserConfig  map[string]option `json:"userConfig"`
	}
	read(pluginPath(t, ".claude-plugin", "plugin.json"), &plugin)
	// No fixed version: Claude Code then tracks the commit, so an update
	// reaches installed copies without a manual bump.
	if plugin.Name != ClaudePluginName || plugin.Description == "" || plugin.Version != nil {
		t.Fatalf("plugin = %+v", plugin)
	}
	if len(plugin.UserConfig) != 3 {
		t.Fatalf("userConfig = %+v", plugin.UserConfig)
	}
	for key, want := range map[string]option{
		// Optional: the plugin is installed before any server exists, and its
		// session hint then guides to /oberth:install.
		"url":    {},
		"token":  {Sensitive: true},
		"ca_pin": {},
	} {
		got := plugin.UserConfig[key]
		if got.Type != "string" || got.Title == "" || got.Description == "" || got.Sensitive != want.Sensitive || got.Required != want.Required {
			t.Errorf("userConfig.%s = %+v", key, got)
		}
	}

	var servers struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	read(pluginPath(t, ".mcp.json"), &servers)
	server := servers.MCPServers[claudeCodeServerName]
	if len(servers.MCPServers) != 1 || server["type"] != "http" || server["timeout"] != float64(ClaudeCodeMCPTimeoutMS) {
		t.Fatalf(".mcp.json = %+v", servers)
	}
	if server["url"] != "${user_config.url}" {
		t.Fatalf("url = %v", server["url"])
	}
	if !reflect.DeepEqual(server["headers"], map[string]any{"Authorization": "Bearer ${user_config.token}"}) {
		t.Fatalf("headers = %v", server["headers"])
	}
	// Claude Code refuses ${user_config.*} in a headersHelper, and a helper
	// would need a token file: the header is the one credential path.
	if _, helper := server["headersHelper"]; helper {
		t.Fatal(".mcp.json carries a headersHelper")
	}
	if entries, err := os.ReadDir(pluginPath(t, "scripts")); err != nil || len(entries) != 4 || entries[0].Name() != "fetch-oberth" || entries[1].Name() != "install" || entries[2].Name() != "trust" || entries[3].Name() != "unseal" {
		t.Fatalf("scripts = %v %v", entries, err)
	}

	var hooks struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string   `json:"type"`
				Command string   `json:"command"`
				Args    []string `json:"args"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	read(pluginPath(t, "hooks", "hooks.json"), &hooks)
	start := hooks.Hooks["SessionStart"]
	if len(hooks.Hooks) != 1 || len(start) != 1 || len(start[0].Hooks) != 1 {
		t.Fatalf("hooks = %+v", hooks)
	}
	// Exec form: the script path is one argument, with no shell quoting.
	if hook := start[0].Hooks[0]; hook.Type != "command" || hook.Command != "sh" || !reflect.DeepEqual(hook.Args, []string{"${CLAUDE_PLUGIN_ROOT}/scripts/trust", "hint"}) {
		t.Fatalf("SessionStart hook = %+v", hook)
	}

	for skill, wants := range map[string][]string{
		"oberth-agent-loop": {
			"name: oberth-agent-loop", "description:",
			`events_wait {"kinds": ["run.held", "run.finished"], "own": true, "after_id": <cursor>, "timeout_s": 600}`,
			"next_after_id", "at or below", "run_wait", "abandon_at", "never evidence", "/oberth:connect",
		},
		"install": {
			"name: install", "description:", "$ARGUMENTS", "--secrets-file", "--context", "--expected-host-fingerprint",
			"Never pick one yourself", "Never print, read, grep, copy or upload the secrets file", "scripts/fetch-oberth", "allowed-tools:", "trust on first",
			"never interrupt it", "Never delete, move, truncate or overwrite a secrets file", "exactly as printed", "--replace-client-files",
		},
		"connect": {
			"name: connect", "description:", `sh "${CLAUDE_PLUGIN_ROOT}/scripts/trust" connect --url`,
			"${user_config.url}", "${user_config.ca_pin}", "$ARGUMENTS", "no trust on first use", "restart Claude Code once",
			"Check both values before any command is written", "exactly 64 hex digits", "`. _ ~ : / % @ [ ] -`", "run nothing", "`Warning:`",
		},
	} {
		body, err := os.ReadFile(pluginPath(t, "skills", skill, "SKILL.md"))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range wants {
			if !strings.Contains(string(body), want) {
				t.Errorf("skills/%s/SKILL.md lacks %q", skill, want)
			}
		}
		// A sensitive option is never substituted into skill text.
		if strings.Contains(string(body), "${user_config.token}") {
			t.Errorf("skills/%s/SKILL.md references the token", skill)
		}
	}
}

// trustShells are the shells the script is checked under: its #!/bin/sh and,
// where installed, bash (the sh of macOS).
func trustShells() []string {
	shells := []string{"sh"}
	if _, err := exec.LookPath("bash"); err == nil {
		shells = append(shells, "bash")
	}
	return shells
}

// runTrust runs the plugin's trust script with Claude Code's directory at
// claudeDir. HOME is a separate empty directory: nothing may be written
// outside claudeDir.
func runTrust(t *testing.T, shell, claudeDir string, environment []string, args ...string) (string, error) {
	t.Helper()
	command := exec.Command(shell, append([]string{pluginPath(t, "scripts", "trust")}, args...)...)
	// The caller's PATH finds sh, awk and openssl; nothing of a real Claude
	// Code session or Oberth client configuration is passed on.
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, "CLAUDE_") && !strings.HasPrefix(variable, "OBERTH_") && !strings.HasPrefix(variable, "NODE_") {
			command.Env = append(command.Env, variable)
		}
	}
	command.Env = append(command.Env, "HOME="+t.TempDir(), "TMPDIR="+t.TempDir(), "CLAUDE_CONFIG_DIR="+claudeDir)
	command.Env = append(command.Env, environment...)
	out, err := command.CombinedOutput()
	return string(out), err
}

// servePresenting starts a TLS server that presents the given chain and
// returns its https://localhost:<port>/mcp address.
func servePresenting(t *testing.T, key *ecdsa.PrivateKey, chain ...[]byte) string {
	t.Helper()
	server := httptest.NewUnstartedServer(http.NotFoundHandler())
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: chain, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return "https://localhost:" + port + "/mcp"
}

func requireOpenSSL(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl is not installed")
	}
}

func emptyDirectory(t *testing.T, path string) bool {
	t.Helper()
	entries, err := os.ReadDir(path)
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(entries) == 0
}

// /oberth:connect trusts nothing on first use: a CA is accepted only when
// its SHA-256 equals the pin, the server must hold a leaf that CA issued,
// and on any refusal Claude Code's directory is left as it was.
func TestPluginConnectRefusesWithoutAMatchingPin(t *testing.T) {
	requireOpenSSL(t)
	authority := newTestAuthority(t, "Oberth test CA")
	other := newTestAuthority(t, "Another CA")
	leaf, key := authority.issue(t)
	address := servePresenting(t, key, leaf, authority.certificate.Raw)
	pin := CertificatePin(authority.certificate)
	otherFile := filepath.Join(t.TempDir(), "other.pem")
	if err := os.WriteFile(otherFile, other.pem, 0o600); err != nil {
		t.Fatal(err)
	}
	// A server whose leaf the pinned CA did not issue, presenting that CA.
	forged, forgedKey := other.issue(t)
	impostor := servePresenting(t, forgedKey, forged, authority.certificate.Raw)
	leafOnly := servePresenting(t, key, leaf)

	for _, shell := range trustShells() {
		for name, test := range map[string]struct {
			args []string
			want string
		}{
			"no pin":                  {[]string{"--url", address, "--pin", ""}, "no CA pin"},
			"unsubstituted pin":       {[]string{"--url", address, "--pin", "${user_config.ca_pin}"}, "no CA pin"},
			"short pin":               {[]string{"--url", address, "--pin", pin[:62]}, "no CA pin"},
			"another CA's pin":        {[]string{"--url", address, "--pin", CertificatePin(other.certificate)}, "refused: no certificate presented"},
			"one digit off":           {[]string{"--url", address, "--pin", flipDigit(pin)}, "refused: no certificate presented"},
			"the leaf's own pin":      {[]string{"--url", address, "--pin", testPin(t, pemOf(leaf))}, "is not a CA certificate"},
			"leaf of another CA":      {[]string{"--url", impostor, "--pin", pin}, "is not issued by the pinned CA"},
			"CA not presented":        {[]string{"--url", leafOnly, "--pin", pin}, "pass --ca-file"},
			"CA file of another CA":   {[]string{"--url", address, "--pin", pin, "--ca-file", otherFile}, "refused: no certificate in"},
			"plain http":              {[]string{"--url", strings.Replace(address, "https://", "http://", 1), "--pin", pin}, "must start with https://"},
			"no server":               {[]string{"--url", "https://localhost:1/mcp", "--pin", pin}, "no certificate was read"},
			"an option for the shell": {[]string{"--url", "https://-oProxyCommand/mcp", "--pin", pin}, "no usable host"},
			"a quote in the URL":      {[]string{"--url", address + "'; touch x; '", "--pin", pin}, "may hold only letters"},
			"a command in the URL":    {[]string{"--url", "https://$(touch x)/mcp", "--pin", pin}, "may hold only letters"},
			"a space in the URL":      {[]string{"--url", "https://localhost /mcp", "--pin", pin}, "may hold only letters"},
			"a query in the URL":      {[]string{"--url", address + "?a=b&c", "--pin", pin}, "may hold only letters"},
			"a command in the pin":    {[]string{"--url", address, "--pin", pin[:32] + "$(touch x)" + pin[32:]}, "no CA pin"},
		} {
			t.Run(shell+" "+name, func(t *testing.T) {
				claudeDir := filepath.Join(t.TempDir(), ".claude")
				out, err := runTrust(t, shell, claudeDir, nil, append([]string{"connect"}, test.args...)...)
				if err == nil || !strings.Contains(out, test.want) || !strings.Contains(out, "Nothing was changed.") {
					t.Fatalf("connect = %v\n%s", err, out)
				}
				if !emptyDirectory(t, claudeDir) {
					t.Fatalf("a refused connect wrote into %s", claudeDir)
				}
			})
		}
	}
}

func flipDigit(pin string) string {
	if pin[0] == '0' {
		return "1" + pin[1:]
	}
	return "0" + pin[1:]
}

func pemOf(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// With the right pin the CA is stored under Claude Code's directory and
// env.NODE_EXTRA_CA_CERTS is merged into settings.json without touching any
// other setting. Each case is also given to `oberth client setup`'s own
// merge: both must leave the same settings and the same trusted bundle.
func TestPluginConnectStoresTheCAAndMergesSettings(t *testing.T) {
	requireOpenSSL(t)
	authority := newTestAuthority(t, "Oberth test CA")
	leaf, key := authority.issue(t)
	address := servePresenting(t, key, leaf, authority.certificate.Raw)
	pin := CertificatePin(authority.certificate)
	other := newTestAuthority(t, "Corporate CA")
	shared := t.TempDir()
	holding := filepath.Join(shared, "holding.pem")
	lacking := filepath.Join(shared, "lacking.pem")
	if err := os.WriteFile(holding, append(append([]byte{}, other.pem...), authority.pem...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lacking, other.pem, 0o644); err != nil {
		t.Fatal(err)
	}

	const missing = "\x00missing"
	for name, test := range map[string]struct {
		settings string
		// trusted is what NODE_EXTRA_CA_CERTS names afterwards, relative
		// to Claude Code's directory unless absolute.
		trusted string
		// kept are fragments of the original that must survive, in order.
		kept []string
	}{
		"no settings file": {missing, "oberth/ca.crt", nil},
		"empty file":       {"", "oberth/ca.crt", nil},
		"empty object":     {"{}\n", "oberth/ca.crt", nil},
		"other settings": {
			"{\n  \"model\": \"opus\",\n  \"permissions\": {\"allow\": [\"Bash(ls:*)\"]}\n}\n", "oberth/ca.crt",
			[]string{"\"model\": \"opus\",\n  \"permissions\": {\"allow\": [\"Bash(ls:*)\"]}\n}\n"},
		},
		"an env with other variables": {
			"{\"env\":{\"FOO\":\"bar\",\"PATH_EXTRA\":\"/a b\"},\"model\":\"opus\"}", "oberth/ca.crt",
			[]string{"\"FOO\":\"bar\",\"PATH_EXTRA\":\"/a b\"},\"model\":\"opus\"}"},
		},
		"an empty env": {"{\n  \"env\": {},\n  \"model\": \"opus\"\n}\n", "oberth/ca.crt", []string{",\n  \"model\": \"opus\"\n}\n"}},
		"an unset variable": {
			"{\"env\": {\"NODE_EXTRA_CA_CERTS\": \"\", \"FOO\": \"bar\"}}", "oberth/ca.crt", []string{", \"FOO\": \"bar\"}}"},
		},
		"decoys and awkward strings": {
			"{\n  \"statusLine\": \"a \\\"} { [ \\\\\",\n  \"hooks\": {\"env\": {\"NODE_EXTRA_CA_CERTS\": \"decoy\"}},\n  \"list\": [{\"env\": {}}, \"env\"],\n  \"env\": {\n    \"FOO\": \"{\\\"env\\\":1}\"\n  },\n  \"tail\": \"env\"\n}\n",
			"oberth/ca.crt",
			[]string{"\"statusLine\": \"a \\\"} { [ \\\\\",\n  \"hooks\": {\"env\": {\"NODE_EXTRA_CA_CERTS\": \"decoy\"}},\n  \"list\": [{\"env\": {}}, \"env\"],\n  \"env\": {", "\n    \"FOO\": \"{\\\"env\\\":1}\"\n  },\n  \"tail\": \"env\"\n}\n"},
		},
		"a bundle that holds the CA": {
			"{\"env\": {\"NODE_EXTRA_CA_CERTS\": \"" + holding + "\"}, \"model\": \"opus\"}\n", holding,
			[]string{"{\"env\": {\"NODE_EXTRA_CA_CERTS\": \"" + holding + "\"}, \"model\": \"opus\"}\n"},
		},
		"a bundle without the CA": {
			"{\"env\": {\"NODE_EXTRA_CA_CERTS\": \"" + lacking + "\"}, \"model\": \"opus\"}\n", "oberth/bundle.pem",
			[]string{"{\"env\": {\"NODE_EXTRA_CA_CERTS\": \"", "\"}, \"model\": \"opus\"}\n"},
		},
	} {
		for _, shell := range trustShells() {
			t.Run(shell+" "+name, func(t *testing.T) {
				claudeDir := filepath.Join(t.TempDir(), ".claude")
				settings := filepath.Join(claudeDir, "settings.json")
				reference := claudeCodeFiles{settings: filepath.Join(t.TempDir(), "settings.json")}
				reference.store = filepath.Join(filepath.Dir(reference.settings), "oberth")
				if err := os.MkdirAll(claudeDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if test.settings != missing {
					for _, path := range []string{settings, reference.settings} {
						if err := os.WriteFile(path, []byte(test.settings), 0o640); err != nil {
							t.Fatal(err)
						}
					}
				}
				// A pin is accepted with colons, in upper case, with a prefix.
				given := "sha256:" + strings.ToUpper(pin[:2]) + ":" + pin[2:]
				out, err := runTrust(t, shell, claudeDir, nil, "connect", "--url", address, "--pin", given)
				if err != nil {
					t.Fatalf("connect: %v\n%s", err, out)
				}
				if !strings.Contains(out, "Restart Claude Code once") || !strings.Contains(out, "trust store was not touched") {
					t.Errorf("connect does not ask for the restart:\n%s", out)
				}
				want := test.trusted
				if !filepath.IsAbs(want) {
					want = filepath.Join(claudeDir, filepath.FromSlash(want))
				}
				body := mustRead(t, settings)
				var merged map[string]any
				if err := json.Unmarshal(body, &merged); err != nil {
					t.Fatalf("settings.json is not JSON any more: %v\n%s", err, body)
				}
				environment, _ := merged["env"].(map[string]any)
				if environment["NODE_EXTRA_CA_CERTS"] != want {
					t.Fatalf("NODE_EXTRA_CA_CERTS = %v, want %s\n%s", environment["NODE_EXTRA_CA_CERTS"], want, body)
				}
				if !inOrder(string(body), test.kept) {
					t.Errorf("settings were reformatted:\n%s\nwant, in order: %q", body, test.kept)
				}
				if test.settings != missing && test.settings != "" {
					if info, err := os.Stat(settings); err != nil || info.Mode().Perm() != 0o640 {
						t.Errorf("settings mode = %v %v", info, err)
					}
				}
				stored := parsePEMCertificates(mustRead(t, filepath.Join(claudeDir, "oberth", "ca.crt")))
				if len(stored) != 1 || !stored[0].Equal(authority.certificate) {
					t.Fatalf("oberth/ca.crt holds %d certificates, want the pinned CA", len(stored))
				}
				if got := strings.TrimSpace(string(mustRead(t, filepath.Join(claudeDir, "oberth", "ca.sha256")))); got != pin {
					t.Errorf("oberth/ca.sha256 = %q", got)
				}
				trusted := parsePEMCertificates(mustRead(t, want))
				if !containsAll(trusted, stored) {
					t.Fatal("the bundle Claude Code is pointed at does not hold the CA")
				}

				// `oberth client setup` merges the same way.
				if _, err := ensureSettingsCATrust(reference, authority.pem, stored, pin, false); err != nil {
					t.Fatal(err)
				}
				var expected map[string]any
				rebased := strings.ReplaceAll(string(mustRead(t, reference.settings)), filepath.Dir(reference.settings), claudeDir)
				if err := json.Unmarshal([]byte(rebased), &expected); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(merged, expected) {
					t.Errorf("the script and oberth client setup disagree:\n%s\n%s", body, rebased)
				}
				referenceTrusted := strings.Replace(want, claudeDir, filepath.Dir(reference.settings), 1)
				if held := parsePEMCertificates(mustRead(t, referenceTrusted)); len(held) != len(trusted) || !containsAll(held, trusted) {
					t.Errorf("the bundles differ: %d and %d certificates", len(trusted), len(held))
				}
				if strings.HasSuffix(want, "bundle.pem") {
					if len(trusted) != 2 || !containsAll(trusted, []*x509.Certificate{other.certificate}) {
						t.Errorf("bundle.pem holds %d certificates, want the existing one too", len(trusted))
					}
					if got := strings.TrimSpace(string(mustRead(t, filepath.Join(claudeDir, "oberth", "bundle.source")))); got != lacking {
						t.Errorf("bundle.source = %q", got)
					}
				}

				// A second run changes nothing, and the session hint is quiet.
				out, err = runTrust(t, shell, claudeDir, nil, "connect", "--url", address, "--pin", pin)
				if err != nil || !strings.Contains(out, "not changed") {
					t.Fatalf("second connect: %v\n%s", err, out)
				}
				if again := mustRead(t, settings); string(again) != string(body) {
					t.Errorf("a second connect rewrote the settings:\n%s", again)
				}
				hint, err := runTrust(t, shell, claudeDir, []string{"CLAUDE_PLUGIN_OPTION_URL=" + address, "CLAUDE_PLUGIN_OPTION_CA_PIN=" + pin}, "hint")
				if err != nil || !strings.Contains(hint, "sealed: run /oberth:unseal") {
					t.Errorf("hint after connect = %q %v", hint, err)
				}
			})
		}
	}
}

// Settings the script cannot edit safely are refused and left byte for
// byte; `oberth client setup` refuses the same files.
func TestPluginConnectRefusesSettingsItCannotMerge(t *testing.T) {
	requireOpenSSL(t)
	authority := newTestAuthority(t, "Oberth test CA")
	leaf, key := authority.issue(t)
	address := servePresenting(t, key, leaf, authority.certificate.Raw)
	pin := CertificatePin(authority.certificate)
	for name, settings := range map[string]string{
		"an array":                     "[1]\n",
		"env is an array":              "{\"env\": []}\n",
		"the value is a number":        "{\"env\": {\"NODE_EXTRA_CA_CERTS\": 5}}\n",
		"truncated":                    "{\"model\": \"opus\", \"env\": {",
		"trailing data":                "{} {}\n",
		"a comment":                    "{\n  // note\n  \"model\": \"opus\"\n}\n",
		"a trailing comma":             "{\"model\": \"opus\",}\n",
		"a trailing comma in a list":   "{\"list\": [1,], \"env\": {}}\n",
		"a word that is no value":      "{\"model\": tru}\n",
		"a missing colon":              "{\"model\" \"opus\"}\n",
		"brackets out of order":        "{\"list\": [}]}\n",
		"a raw tab in a string":        "{\"model\": \"a\tb\"}\n",
		"an unknown escape":            "{\"model\": \"a\\xb\"}\n",
		"a number with a leading zero": "{\"n\": 01}\n",
		"single quotes":                "{'model': 'opus'}\n",
		"an unreadable bundle":         "{\"env\": {\"NODE_EXTRA_CA_CERTS\": \"/nonexistent/bundle.pem\"}}\n",
	} {
		for _, shell := range trustShells() {
			t.Run(shell+" "+name, func(t *testing.T) {
				claudeDir := filepath.Join(t.TempDir(), ".claude")
				path := filepath.Join(claudeDir, "settings.json")
				if err := os.MkdirAll(claudeDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(settings), 0o600); err != nil {
					t.Fatal(err)
				}
				out, err := runTrust(t, shell, claudeDir, nil, "connect", "--url", address, "--pin", pin)
				if err == nil || !strings.Contains(out, "Nothing was changed.") {
					t.Fatalf("connect = %v\n%s", err, out)
				}
				if string(mustRead(t, path)) != settings {
					t.Fatal("refused settings were changed")
				}
				if _, err := os.Stat(filepath.Join(claudeDir, "oberth")); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("a refused connect stored a CA: %v", err)
				}
				reference := claudeCodeFiles{settings: path, store: filepath.Join(claudeDir, "oberth")}
				if _, err := ensureSettingsCATrust(reference, authority.pem, []*x509.Certificate{authority.certificate}, pin, false); err == nil {
					t.Fatal("oberth client setup merged settings the script refuses")
				}
			})
		}
	}
}

// A server that presents only its leaf: the CA then comes from a file,
// checked against the same pin and the same server.
func TestPluginConnectTakesTheCAFromAFile(t *testing.T) {
	requireOpenSSL(t)
	authority := newTestAuthority(t, "Oberth test CA")
	leaf, key := authority.issue(t)
	address := servePresenting(t, key, leaf)
	file := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(file, authority.pem, 0o600); err != nil {
		t.Fatal(err)
	}
	claudeDir := filepath.Join(t.TempDir(), ".claude")
	out, err := runTrust(t, "sh", claudeDir, nil, "connect", "--url", address, "--pin", CertificatePin(authority.certificate), "--ca-file", file)
	if err != nil {
		t.Fatalf("connect: %v\n%s", err, out)
	}
	stored := parsePEMCertificates(mustRead(t, filepath.Join(claudeDir, "oberth", "ca.crt")))
	if len(stored) != 1 || !stored[0].Equal(authority.certificate) {
		t.Fatal("the CA of the file was not stored")
	}
}

// The SessionStart hook tells Claude to offer /oberth:connect while the CA
// is not trusted, says nothing once it is, never prints the token it finds
// in its environment, and never fails.
func TestPluginSessionHint(t *testing.T) {
	authority := newTestAuthority(t, "Oberth test CA")
	pin := CertificatePin(authority.certificate)
	const address = "https://user:hunter2@oberth.example.test:30443/mcp"
	for _, shell := range trustShells() {
		t.Run(shell, func(t *testing.T) {
			claudeDir := filepath.Join(t.TempDir(), ".claude")
			hint := func(pin string) string {
				t.Helper()
				out, err := runTrust(t, shell, claudeDir, []string{"CLAUDE_PLUGIN_OPTION_URL=" + address, "CLAUDE_PLUGIN_OPTION_CA_PIN=" + pin, "CLAUDE_PLUGIN_OPTION_TOKEN=" + testToken}, "hint")
				if err != nil {
					t.Fatalf("hint failed: %v\n%s", err, out)
				}
				if strings.Contains(out, testToken) || strings.Contains(out, "hunter2") {
					t.Fatal("the hint prints a credential")
				}
				return out
			}
			if out := hint(pin); !inOrder(out, []string{"does not trust the CA", "https://oberth.example.test:30443/mcp", "/oberth:connect", "restart", "Do not disable TLS verification"}) {
				t.Fatalf("hint without trust = %q", out)
			}
			if out := hint("not-a-pin"); !strings.Contains(out, "not a SHA-256 fingerprint") {
				t.Fatalf("hint with a malformed pin = %q", out)
			}
			// Without a pin the server has a publicly trusted certificate.
			if out := hint(""); !strings.Contains(out, "sealed: run /oberth:unseal") {
				t.Fatalf("hint without a pin = %q", out)
			}
			// An unconfigured plugin has nothing to connect to.
			// Without a server the hint guides to an install or a connection,
			// and asks for nothing secret in the chat.
			if out, err := runTrust(t, shell, claudeDir, nil, "hint"); err != nil || !inOrder(out, []string{"no Oberth server is configured", "/oberth:install", "/plugin configure", "never into the chat", "/oberth:connect"}) {
				t.Fatalf("hint without a URL = %q %v", out, err)
			}

			// What `oberth client setup` writes counts as trust.
			paths := claudeCodeFiles{settings: filepath.Join(claudeDir, "settings.json"), store: filepath.Join(claudeDir, "oberth")}
			if _, err := ensureSettingsCATrust(paths, authority.pem, []*x509.Certificate{authority.certificate}, pin, false); err != nil {
				t.Fatal(err)
			}
			if out := hint(pin); !strings.Contains(out, "sealed: run /oberth:unseal") {
				t.Fatalf("hint with trust = %q", out)
			}
			if out := hint(strings.ToUpper(pin)); !strings.Contains(out, "sealed: run /oberth:unseal") {
				t.Fatalf("hint with the pin in upper case = %q", out)
			}
			// Another pin (a rotated CA) is not the stored one.
			if out := hint(flipDigit(pin)); !strings.Contains(out, "/oberth:connect") {
				t.Fatalf("hint with another pin = %q", out)
			}
			// Settings that no longer name the CA.
			if err := os.WriteFile(paths.settings, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if out := hint(pin); !strings.Contains(out, "/oberth:connect") {
				t.Fatalf("hint with the setting removed = %q", out)
			}
			// Broken settings never fail the session.
			if err := os.WriteFile(paths.settings, []byte("{\"env\": ["), 0o600); err != nil {
				t.Fatal(err)
			}
			if out := hint(pin); !strings.Contains(out, "/oberth:connect") {
				t.Fatalf("hint with broken settings = %q", out)
			}
		})
	}
}

// The leaf is checked against the pinned CA alone: a CA that openssl trusts
// by default (here through SSL_CERT_DIR) does not make another CA's leaf
// pass, even when the server also presents the pinned CA.
func TestPluginConnectIgnoresTheDefaultTrustOfOpenSSL(t *testing.T) {
	requireOpenSSL(t)
	authority := newTestAuthority(t, "Oberth test CA")
	other := newTestAuthority(t, "A default CA")
	forged, forgedKey := other.issue(t)
	impostor := servePresenting(t, forgedKey, forged, authority.certificate.Raw)
	defaults := t.TempDir()
	otherFile := filepath.Join(defaults, "other.pem")
	if err := os.WriteFile(otherFile, other.pem, 0o600); err != nil {
		t.Fatal(err)
	}
	hash, err := exec.Command("openssl", "x509", "-in", otherFile, "-noout", "-subject_hash").Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(defaults, strings.TrimSpace(string(hash))+".0"), other.pem, 0o600); err != nil {
		t.Fatal(err)
	}
	// The directory does make openssl accept the forged leaf by default.
	leafFile := filepath.Join(t.TempDir(), "leaf.pem")
	if err := os.WriteFile(leafFile, pemOf(forged), 0o600); err != nil {
		t.Fatal(err)
	}
	check := exec.Command("openssl", "verify", leafFile)
	check.Env = append(os.Environ(), "SSL_CERT_DIR="+defaults)
	if out, err := check.CombinedOutput(); err != nil {
		t.Fatalf("the default directory is not in use: %v\n%s", err, out)
	}
	claudeDir := filepath.Join(t.TempDir(), ".claude")
	out, err := runTrust(t, "sh", claudeDir, []string{"SSL_CERT_DIR=" + defaults, "SSL_CERT_FILE=" + otherFile}, "connect", "--url", impostor, "--pin", CertificatePin(authority.certificate))
	if err == nil || !strings.Contains(out, "is not issued by the pinned CA") || !strings.Contains(out, "Nothing was changed.") {
		t.Fatalf("connect = %v\n%s", err, out)
	}
	if !emptyDirectory(t, claudeDir) {
		t.Fatalf("a refused connect wrote into %s", claudeDir)
	}
}

// A key written with an escape could spell env without the script seeing
// it; such settings are refused and left as they were.
func TestPluginConnectRefusesAnEscapedSettingsKey(t *testing.T) {
	requireOpenSSL(t)
	authority := newTestAuthority(t, "Oberth test CA")
	leaf, key := authority.issue(t)
	address := servePresenting(t, key, leaf, authority.certificate.Raw)
	for name, settings := range map[string]string{
		"env":          "{\"en\\u0076\": {\"FOO\": \"bar\"}}\n",
		"the variable": "{\"env\": {\"NODE_EXTRA_CA_\\u0043ERTS\": \"/nonexistent\"}}\n",
	} {
		t.Run(name, func(t *testing.T) {
			claudeDir := filepath.Join(t.TempDir(), ".claude")
			path := filepath.Join(claudeDir, "settings.json")
			if err := os.MkdirAll(claudeDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(settings), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := runTrust(t, "sh", claudeDir, nil, "connect", "--url", address, "--pin", CertificatePin(authority.certificate))
			if err == nil || !strings.Contains(out, "a key with an escape") || !strings.Contains(out, "Nothing was changed.") {
				t.Fatalf("connect = %v\n%s", err, out)
			}
			if string(mustRead(t, path)) != settings {
				t.Fatal("refused settings were changed")
			}
		})
	}
}

// A settings.json that is a symbolic link stays one: /oberth:connect and
// `oberth client setup` both change the file it names.
func TestCATrustKeepsASymbolicLinkToTheSettings(t *testing.T) {
	requireOpenSSL(t)
	authority := newTestAuthority(t, "Oberth test CA")
	leaf, key := authority.issue(t)
	address := servePresenting(t, key, leaf, authority.certificate.Raw)
	pin := CertificatePin(authority.certificate)
	for name, trust := range map[string]func(t *testing.T, claudeDir string){
		"connect": func(t *testing.T, claudeDir string) {
			if out, err := runTrust(t, "sh", claudeDir, nil, "connect", "--url", address, "--pin", pin); err != nil {
				t.Fatalf("connect: %v\n%s", err, out)
			}
		},
		"client setup": func(t *testing.T, claudeDir string) {
			paths := claudeCodeFiles{settings: filepath.Join(claudeDir, "settings.json"), store: filepath.Join(claudeDir, "oberth")}
			if _, err := ensureSettingsCATrust(paths, authority.pem, []*x509.Certificate{authority.certificate}, pin, false); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			claudeDir := filepath.Join(t.TempDir(), ".claude")
			real := filepath.Join(t.TempDir(), "dotfiles", "settings.json")
			for _, directory := range []string{claudeDir, filepath.Dir(real)} {
				if err := os.MkdirAll(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(real, []byte("{\"model\": \"opus\"}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(claudeDir, "settings.json")
			if err := os.Symlink(real, link); err != nil {
				t.Fatal(err)
			}
			trust(t, claudeDir)
			if info, err := os.Lstat(link); err != nil || info.Mode()&fs.ModeSymlink == 0 {
				t.Fatalf("settings.json is not a symbolic link any more: %v %v", info, err)
			}
			var merged struct {
				Model string            `json:"model"`
				Env   map[string]string `json:"env"`
			}
			if err := json.Unmarshal(mustRead(t, real), &merged); err != nil {
				t.Fatal(err)
			}
			if merged.Model != "opus" || merged.Env["NODE_EXTRA_CA_CERTS"] != filepath.Join(claudeDir, "oberth", "ca.crt") {
				t.Fatalf("the linked file = %+v", merged)
			}
		})
	}
}

// A CA without critical name constraints is trusted by Claude Code for
// every host: /oberth:connect and `oberth client setup` both store it and
// say so in the same line. The Oberth server CA, which is constrained to
// the server's names, gets no warning.
func TestCATrustWarnsAboutACAWithoutNameConstraints(t *testing.T) {
	requireOpenSSL(t)
	for name, test := range map[string]struct {
		authority testAuthority
		warned    bool
	}{
		"constrained":   {newTestAuthorityFor(t, "Oberth test CA", "localhost"), false},
		"unconstrained": {newTestAuthority(t, "Any CA"), true},
	} {
		t.Run(name, func(t *testing.T) {
			leaf, key := test.authority.issue(t)
			address := servePresenting(t, key, leaf, test.authority.certificate.Raw)
			claudeDir := filepath.Join(t.TempDir(), ".claude")
			out, err := runTrust(t, "sh", claudeDir, nil, "connect", "--url", address, "--pin", CertificatePin(test.authority.certificate))
			if err != nil {
				t.Fatalf("connect: %v\n%s", err, out)
			}
			if got := strings.Contains(out, UnconstrainedCAWarning+"\n"); got != test.warned || strings.Contains(out, "Warning:") != test.warned {
				t.Errorf("connect warned = %v, want %v:\n%s", got, test.warned, out)
			}
			if stored := parsePEMCertificates(mustRead(t, filepath.Join(claudeDir, "oberth", "ca.crt"))); len(stored) != 1 {
				t.Fatal("the CA was not stored")
			}

			reference := t.TempDir()
			paths := claudeCodeFiles{settings: filepath.Join(reference, "settings.json"), store: filepath.Join(reference, "oberth")}
			for _, run := range []string{"first", "again"} {
				change, _, err := ensureClaudeCodeCATrust(paths, ClaudeCodeSetup{CA: string(test.authority.pem)})
				if err != nil {
					t.Fatal(err)
				}
				if got := strings.Contains(change.String(), UnconstrainedCAWarning); got != test.warned {
					t.Errorf("client setup (%s) warned = %v, want %v:\n%s", run, got, test.warned, change.String())
				}
			}
		})
	}
}
