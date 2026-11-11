package client

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolate clears every connection variable and points the client files at
// an empty directory, returning that directory.
func isolate(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"OBERTH_BASE_URL", "OBERTH_MCP_URL", "OBERTH_TOKEN", "OBERTH_TOKEN_COMMAND", "OBERTH_CA_CERT"} {
		t.Setenv(name, "")
	}
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	directory := filepath.Join(root, "oberth")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func write(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func TestResolveConnectionNamesEveryWayWhenNothingIsConfigured(t *testing.T) {
	isolate(t)
	_, err := ResolveConnection(ConnectionFlags{})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{"--url", "OBERTH_BASE_URL", "OBERTH_TOKEN_COMMAND", "~/.config/oberth"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
}

// The files oberth install writes are enough: env names the server and the
// token command, ca.crt the CA; a saved token file serves without either.
func TestResolveConnectionReadsTheClientFiles(t *testing.T) {
	directory := isolate(t)
	write(t, filepath.Join(directory, "ca.crt"), "pem", 0o644)
	write(t, filepath.Join(directory, "token"), "file-token\n", 0o600)
	connection, err := ResolveConnection(ConnectionFlags{})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("a token alone named a server: %+v %v", connection, err)
	}
	write(t, filepath.Join(directory, "mcp.json"), `{"mcpServers":{"oberth":{"type":"http","url":"https://ci.example.test:30443/mcp"}}}`, 0o644)
	connection, err = ResolveConnection(ConnectionFlags{})
	if err != nil || connection.BaseURL != "https://ci.example.test:30443" || connection.MCPURL != "https://ci.example.test:30443/mcp" ||
		connection.Token != "file-token" || connection.CACert != filepath.Join(directory, "ca.crt") {
		t.Fatalf("mcp.json + token = %+v %v", connection, err)
	}
	write(t, filepath.Join(directory, "env"), "export OBERTH_BASE_URL=\"https://env.example.test:30443\"\nexport OBERTH_TOKEN_COMMAND=\"printf env-token\"\nexport OBERTH_CA_CERT=\"/env/ca.crt\"\n", 0o644)
	connection, err = ResolveConnection(ConnectionFlags{})
	if err != nil || connection.BaseURL != "https://env.example.test:30443" || connection.MCPURL != "https://env.example.test:30443/mcp" ||
		connection.Token != "" || connection.TokenCommand != "printf env-token" || connection.CACert != "/env/ca.crt" {
		t.Fatalf("env file = %+v %v", connection, err)
	}
}

// Precedence per setting: flag, then environment, then the client files.
func TestResolveConnectionPrecedenceFlagEnvironmentFiles(t *testing.T) {
	directory := isolate(t)
	write(t, filepath.Join(directory, "env"), "export OBERTH_BASE_URL=\"https://file.example.test\"\nexport OBERTH_TOKEN_COMMAND=\"printf file\"\n", 0o644)
	t.Setenv("OBERTH_BASE_URL", "https://env.example.test:8443")
	t.Setenv("OBERTH_TOKEN", "env-token")
	t.Setenv("OBERTH_CA_CERT", "/env/ca.crt")
	connection, err := ResolveConnection(ConnectionFlags{})
	if err != nil || connection.BaseURL != "https://env.example.test:8443" || connection.MCPURL != "https://env.example.test:8443/mcp" ||
		connection.Token != "env-token" || !connection.TokenFromEnvironment || connection.CACert != "/env/ca.crt" {
		t.Fatalf("environment = %+v %v", connection, err)
	}
	t.Setenv("OBERTH_MCP_URL", "https://proxy.example.test/oberth/mcp")
	if connection, err = ResolveConnection(ConnectionFlags{}); err != nil || connection.MCPURL != "https://proxy.example.test/oberth/mcp" || connection.BaseURL != "https://env.example.test:8443" {
		t.Fatalf("both variables = %+v %v", connection, err)
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	write(t, tokenFile, "flag-token", 0o600)
	connection, err = ResolveConnection(ConnectionFlags{URL: "https://flag.example.test:30443/mcp", TokenFile: tokenFile, CAFile: "/flag/ca.crt"})
	if err != nil || connection.BaseURL != "https://flag.example.test:30443" || connection.MCPURL != "https://flag.example.test:30443/mcp" ||
		connection.Token != "flag-token" || connection.TokenFromEnvironment || connection.CACert != "/flag/ca.crt" {
		t.Fatalf("flags = %+v %v", connection, err)
	}
	if connection, err = ResolveConnection(ConnectionFlags{URL: "https://flag.example.test:30443/"}); err != nil || connection.MCPURL != "https://flag.example.test:30443/mcp" {
		t.Fatalf("base address flag = %+v %v", connection, err)
	}
}

func TestResolveConnectionRefusesUnsafeTokenFilesAndCleartext(t *testing.T) {
	directory := isolate(t)
	write(t, filepath.Join(directory, "env"), "export OBERTH_BASE_URL=\"https://ci.example.test\"\n", 0o600)
	write(t, filepath.Join(directory, "token"), "file-token", 0o644)
	if _, err := ResolveConnection(ConnectionFlags{URL: "https://ci.example.test"}); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("group-readable saved token = %v", err)
	}
	if err := os.Remove(filepath.Join(directory, "token")); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveConnection(ConnectionFlags{URL: "https://ci.example.test"}); err == nil || !strings.Contains(err.Error(), "no Oberth credential") {
		t.Fatalf("no credential = %v", err)
	}
	t.Setenv("OBERTH_TOKEN", "x")
	if _, err := ResolveConnection(ConnectionFlags{URL: "http://ci.example.test"}); err == nil || !strings.Contains(err.Error(), "cleartext") {
		t.Fatalf("plain http = %v", err)
	}
}

// A saved token and CA belong to the server the client files name; a flag or
// variable naming another address never gets them (review P2-2).
func TestSavedCredentialNeverFollowsAnotherAddress(t *testing.T) {
	directory := isolate(t)
	write(t, filepath.Join(directory, "env"), "export OBERTH_BASE_URL=\"https://a.example.test:30443\"\nexport OBERTH_CA_CERT=\"/a/ca.crt\"\n", 0o600)
	write(t, filepath.Join(directory, "token"), "token-of-a", 0o600)
	write(t, filepath.Join(directory, "ca.crt"), "pem", 0o644)
	// The same server, spelled as its /mcp endpoint, keeps the files.
	connection, err := ResolveConnection(ConnectionFlags{URL: "https://A.example.test:30443/mcp/"})
	if err != nil || connection.Token != "token-of-a" || connection.CACert != "/a/ca.crt" {
		t.Fatalf("same server = %+v %v", connection, err)
	}
	for name, configure := range map[string]func(){
		"flag":     func() {},
		"variable": func() { t.Setenv("OBERTH_BASE_URL", "https://b.example.test") },
	} {
		t.Run(name, func(t *testing.T) {
			configure()
			given := ConnectionFlags{}
			if name == "flag" {
				given.URL = "https://b.example.test"
			}
			connection, err := ResolveConnection(given)
			if err == nil || connection.Token != "" || !strings.Contains(err.Error(), "never sent to another server") ||
				!strings.Contains(err.Error(), "https://a.example.test:30443") || !strings.Contains(err.Error(), "--token-file") {
				t.Fatalf("other server = %+v %v", connection, err)
			}
			t.Setenv("OBERTH_TOKEN", "token-of-b")
			connection, err = ResolveConnection(given)
			if err != nil || connection.Token != "token-of-b" || connection.CACert != "" {
				t.Fatalf("other server with its own token = %+v %v (the files' CA must not apply)", connection, err)
			}
			t.Setenv("OBERTH_TOKEN", "")
		})
	}
	// Files that name no server lend their token to nobody.
	if err := os.Remove(filepath.Join(directory, "env")); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveConnection(ConnectionFlags{URL: "https://a.example.test:30443"}); err == nil || !strings.Contains(err.Error(), "names no server address") {
		t.Fatalf("unnamed saved token = %v", err)
	}
}

// An env file's token command runs only from a file the user owns and only
// the user can change.
func TestEnvFileTokenCommandNeedsAFileOnlyTheUserCanChange(t *testing.T) {
	directory := isolate(t)
	env := filepath.Join(directory, "env")
	write(t, env, "export OBERTH_BASE_URL=\"https://ci.example.test\"\nexport OBERTH_TOKEN_COMMAND=\"printf x\"\n", 0o644)
	if connection, err := ResolveConnection(ConnectionFlags{}); err != nil || connection.TokenCommand != "printf x" {
		t.Fatalf("owner-only env = %+v %v", connection, err)
	}
	if err := os.Chmod(env, 0o664); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveConnection(ConnectionFlags{}); err == nil || !strings.Contains(err.Error(), "writable by others") {
		t.Fatalf("group-writable env = %v", err)
	}
	// A saved CA someone else can change is refused the same way.
	if err := os.Chmod(env, 0o600); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(directory, "ca.crt"), "pem", 0o666)
	if err := os.Chmod(filepath.Join(directory, "ca.crt"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveConnection(ConnectionFlags{}); err == nil || !strings.Contains(err.Error(), "ca.crt is writable by others") {
		t.Fatalf("world-writable ca.crt = %v", err)
	}
}

// A refusal naming two servers never echoes user information from either.
func TestMismatchMessageDropsUserInformation(t *testing.T) {
	directory := isolate(t)
	write(t, filepath.Join(directory, "env"), "export OBERTH_BASE_URL=\"https://saved:hunter2@a.example.test:30443\"\n", 0o600)
	write(t, filepath.Join(directory, "token"), "token-of-a", 0o600)
	_, err := ResolveConnection(ConnectionFlags{URL: "https://other:pass@b.example.test"})
	if err == nil || strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "pass@") || !strings.Contains(err.Error(), "a.example.test:30443") {
		t.Fatalf("mismatch message = %v", err)
	}
}

// OBERTH_BASE_URL and OBERTH_MCP_URL can name two servers; the saved token
// goes to neither unless both are the server the files name.
func TestSavedCredentialNeverFollowsASplitAddress(t *testing.T) {
	directory := isolate(t)
	write(t, filepath.Join(directory, "env"), "export OBERTH_BASE_URL=\"https://a.example.test:30443\"\n", 0o600)
	write(t, filepath.Join(directory, "token"), "token-of-a", 0o600)
	t.Setenv("OBERTH_BASE_URL", "https://a.example.test:30443")
	t.Setenv("OBERTH_MCP_URL", "https://b.example.test/mcp")
	connection, err := ResolveConnection(ConnectionFlags{})
	if err == nil || connection.Token != "" || !strings.Contains(err.Error(), "never sent to another server") {
		t.Fatalf("split address = %+v %v", connection, err)
	}
	t.Setenv("OBERTH_MCP_URL", "https://a.example.test:30443/mcp")
	if connection, err = ResolveConnection(ConnectionFlags{}); err != nil || connection.Token != "token-of-a" {
		t.Fatalf("both the files' server = %+v %v", connection, err)
	}
}

func TestSameServer(t *testing.T) {
	const saved = "https://a.example.test:30443"
	for _, same := range []string{
		saved, saved + "/", saved + "/mcp", saved + "/mcp/", "HTTPS://A.Example.Test:30443/mcp", "https://user@a.example.test:30443",
	} {
		if !SameServer(same, saved) {
			t.Errorf("%s is the saved server", same)
		}
	}
	for _, other := range []string{
		"https://a.example.test", "https://a.example.test:443", "https://a.example.test:30444", "http://a.example.test:30443",
		"https://a.example.test.:30443", "https://a.example.test:30443@b.example.test", "https://a.example.test:30443.b.example.test",
		"https://b.example.test:30443", "https://a.example.test:30443/prefix", "https://xn--a-0ga.example.test:30443", "a.example.test:30443", "",
	} {
		if SameServer(other, saved) {
			t.Errorf("%s is not the saved server", other)
		}
	}
	for left, right := range map[string]string{
		"https://a.example.test":      "https://a.example.test:443/mcp",
		"http://localhost:80":         "http://LOCALHOST/",
		"https://[::1]:443":           "https://[::1]",
		"https://[2001:DB8::1]:30443": "https://[2001:db8::1]:30443/mcp",
	} {
		if !SameServer(left, right) {
			t.Errorf("%s and %s are one server", left, right)
		}
	}
	if SameServer("https://[::1]:30443", "https://[0:0:0:0:0:0:0:1]:30443") || SameServer("https://a.example.test:80", "https://a.example.test") {
		t.Error("another spelling or a non-default port read as the same server")
	}
}

// The client files steer where the saved token goes, so a directory, env,
// mcp.json or token file someone else can change is not read at all.
func TestClientFilesOthersCanChangeAreNotRead(t *testing.T) {
	for _, writable := range []string{"", "env", "mcp.json", "token"} {
		t.Run("writable "+writable, func(t *testing.T) {
			directory := isolate(t)
			write(t, filepath.Join(directory, "env"), "export OBERTH_BASE_URL=\"https://a.example.test\"\n", 0o600)
			write(t, filepath.Join(directory, "mcp.json"), `{"mcpServers":{"oberth":{"url":"https://a.example.test/mcp"}}}`, 0o600)
			write(t, filepath.Join(directory, "token"), "token-of-a", 0o600)
			target := directory
			if writable != "" {
				target = filepath.Join(directory, writable)
			}
			info, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(target, info.Mode().Perm()|0o020); err != nil {
				t.Fatal(err)
			}
			connection, err := ResolveConnection(ConnectionFlags{})
			if err == nil || connection.Token != "" || !strings.Contains(err.Error(), "not trusted") {
				t.Fatalf("files read although %s is group-writable: %+v %v", target, connection, err)
			}
			connection, err = ResolveConnection(ConnectionFlags{URL: "https://a.example.test"})
			if err == nil || connection.Token != "" || !strings.Contains(err.Error(), "not trusted") {
				t.Fatalf("saved token used although %s is group-writable: %+v %v", target, connection, err)
			}
			t.Setenv("OBERTH_TOKEN", "explicit")
			if connection, err := ResolveConnection(ConnectionFlags{URL: "https://a.example.test"}); err != nil || connection.Token != "explicit" {
				t.Fatalf("an explicit credential = %+v %v", connection, err)
			}
		})
	}
}

func TestDisplayAddressDropsUserInformation(t *testing.T) {
	for input, want := range map[string]string{
		"https://user:secret@a.example.test:30443/mcp": "https://a.example.test:30443/mcp",
		"https://a:b@c@host.example.test":              "https://host.example.test",
		"https://host.example.test:30443":              "https://host.example.test:30443",
		"https://host.example.test/":                   "https://host.example.test/",
		"host.example.test":                            "host.example.test",
	} {
		if got := DisplayAddress(input); got != want {
			t.Errorf("DisplayAddress(%q) = %q, want %q", input, got, want)
		}
	}
}
