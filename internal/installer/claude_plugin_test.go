package installer

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/client"
)

// The repository is a Claude Code plugin marketplace with one plugin,
// claude-plugin/: its manifests must parse, name each other and register
// the same MCP entry oberth client setup does (http, 630 s timeout, a
// headersHelper rather than a literal token), with a configurable URL.
func TestClaudePluginManifests(t *testing.T) {
	root := filepath.Join("..", "..")
	read := func(path string, into any) {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(root, path))
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
	read(".claude-plugin/marketplace.json", &marketplace)
	if marketplace.Name == "" || marketplace.Owner.Name == "" || len(marketplace.Plugins) != 1 {
		t.Fatalf("marketplace = %+v", marketplace)
	}
	entry := marketplace.Plugins[0]
	if entry.Name != "oberth" || entry.Source != "./claude-plugin" || entry.Description == "" {
		t.Fatalf("marketplace plugin = %+v", entry)
	}
	var plugin struct {
		Name        string  `json:"name"`
		Description string  `json:"description"`
		Version     *string `json:"version"`
	}
	read("claude-plugin/.claude-plugin/plugin.json", &plugin)
	// No fixed version: Claude Code then tracks the commit, so an update
	// reaches installed copies without a manual bump (review P2-1).
	if plugin.Name != entry.Name || plugin.Description == "" || plugin.Version != nil {
		t.Fatalf("plugin = %+v", plugin)
	}
	var servers struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	read("claude-plugin/.mcp.json", &servers)
	server := servers.MCPServers[claudeCodeServerName]
	if len(servers.MCPServers) != 1 || server["type"] != "http" || server["timeout"] != float64(ClaudeCodeMCPTimeoutMS) {
		t.Fatalf(".mcp.json = %+v", servers)
	}
	if url, _ := server["url"].(string); !strings.HasPrefix(url, "${OBERTH_MCP_URL:-https://") || !strings.HasSuffix(url, "/mcp}") {
		t.Fatalf("url %q is not configured through OBERTH_MCP_URL with an https default", url)
	}
	if helper, _ := server["headersHelper"].(string); helper != "${CLAUDE_PLUGIN_ROOT}/scripts/auth-header" {
		t.Fatalf("headersHelper = %q", helper)
	}
	if _, literal := server["headers"]; literal {
		t.Fatal(".mcp.json carries a literal header")
	}
	skill, err := os.ReadFile(filepath.Join(root, "claude-plugin", "skills", "oberth-agent-loop", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"name: oberth-agent-loop", "description:", "run_wait", "events_wait", "abandon_at", "never evidence"} {
		if !strings.Contains(string(skill), want) {
			t.Errorf("SKILL.md lacks %q", want)
		}
	}
}

// pluginShell, when set, runs the helper under that shell instead of its
// #!/bin/sh, so the comparison is checked under bash too (macOS sh).
var pluginShell string

// pluginHelper runs the plugin's headersHelper with the client files in
// config and the given environment.
func pluginHelper(t *testing.T, config string, environment ...string) (string, error) {
	t.Helper()
	helper, err := filepath.Abs(filepath.Join("..", "..", "claude-plugin", "scripts", "auth-header"))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(helper)
	if pluginShell != "" {
		command = exec.Command(pluginShell, helper)
	}
	command.Env = append([]string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "XDG_CONFIG_HOME=" + config}, environment...)
	out, err := command.Output()
	return string(out), err
}

func pluginFiles(t *testing.T, env string) string {
	t.Helper()
	config := t.TempDir()
	directory := filepath.Join(config, "oberth")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if env != "" {
		if err := os.WriteFile(filepath.Join(directory, "env"), []byte(env), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "token"), []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return config
}

// The headersHelper prints the header from OBERTH_TOKEN_COMMAND for any
// server, else from the owner-only token file, refuses a file others can
// read and a token that would need JSON escaping.
func TestClaudePluginHeadersHelper(t *testing.T) {
	helper, err := filepath.Abs(filepath.Join("..", "..", "claude-plugin", "scripts", "auth-header"))
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(helper); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("helper not executable: %v %v", info, err)
	}
	if _, err := pluginHelper(t, t.TempDir()); err == nil {
		t.Fatal("helper printed a header without a token")
	}
	config := pluginFiles(t, "export OBERTH_BASE_URL=\"https://localhost:30443\"\n")
	token := filepath.Join(config, "oberth", "token")
	if err := os.Chmod(token, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := pluginHelper(t, config); err == nil {
		t.Fatal("helper read a token file others can read")
	}
	if err := os.Chmod(token, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, unsafe := range []string{`printf 'a"b'`, `printf 'a\\b'`, `printf 'a b'`} {
		if out, err := pluginHelper(t, config, "OBERTH_TOKEN_COMMAND="+unsafe); err == nil {
			t.Fatalf("%s printed %q", unsafe, out)
		}
	}
	for _, test := range []struct {
		environment []string
		want        string
	}{
		{nil, "file-token"},
		{[]string{"OBERTH_TOKEN_COMMAND=printf command-token"}, "command-token"},
		// A token command serves any server.
		{[]string{"OBERTH_TOKEN_COMMAND=printf command-token", "OBERTH_MCP_URL=https://other.example.test/mcp"}, "command-token"},
	} {
		out, err := pluginHelper(t, config, test.environment...)
		var header map[string]string
		if err != nil || json.Unmarshal([]byte(out), &header) != nil || header["Authorization"] != "Bearer "+test.want {
			t.Fatalf("%v: %q %v", test.environment, out, err)
		}
	}
}

// The saved token goes only to the server the client files name, by the
// same rule as the CLI (client.SameServer); on a mismatch stdout stays
// empty (review: the plugin path of P2-2).
func TestClaudePluginHelperSendsTheSavedTokenOnlyToItsServer(t *testing.T) {
	for _, shell := range []string{"", "bash"} {
		if shell != "" {
			if _, err := exec.LookPath(shell); err != nil {
				continue
			}
		}
		t.Run("shell "+shell, func(t *testing.T) {
			pluginShell = shell
			defer func() { pluginShell = "" }()
			helperComparison(t)
		})
	}
}

func helperComparison(t *testing.T) {
	const saved = "https://a.example.test:30443"
	targets := []string{
		saved, saved + "/", saved + "/mcp", saved + "/mcp/", "HTTPS://A.Example.Test:30443/mcp", "https://user@a.example.test:30443/mcp",
		"https://a.example.test", "https://a.example.test:443", "https://a.example.test:30444", "http://a.example.test:30443",
		"https://a.example.test.:30443", "https://a.example.test:30443@b.example.test", "https://a.example.test:30443.b.example.test",
		"https://b.example.test:30443", "https://a.example.test:30443/prefix", "https://xn--a-0ga.example.test:30443", "a.example.test:30443",
		"https://a.example.test:30443/mcp?x=1", "https://a.example.test:30443/%6dcp", "https://a.example.test:30443//mcp",
	}
	for _, savedAs := range []string{
		"export OBERTH_BASE_URL=\"" + saved + "\"\n",
		"export OBERTH_MCP_URL=\"" + saved + "/mcp\"\n",
		"# written by a test\nexport OBERTH_BASE_URL=\"https://old.example.test\"\nexport OBERTH_BASE_URL=\"" + saved + "\"\n",
		"", // mcp.json only
	} {
		config := pluginFiles(t, savedAs)
		if savedAs == "" {
			body := `{"mcpServers":{"oberth":{"type":"http","url":"` + saved + `/mcp","timeout":630000}}}`
			if err := os.WriteFile(filepath.Join(config, "oberth", "mcp.json"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		for _, target := range targets {
			out, err := pluginHelper(t, config, "OBERTH_MCP_URL="+target)
			if want := client.SameServer(target, saved); (err == nil) != want {
				t.Errorf("saved as %q, target %s: helper sent=%v, client.SameServer=%v (%q)", savedAs, target, err == nil, want, out)
			}
			if err != nil && out != "" {
				t.Errorf("target %s: refusal printed %q on stdout", target, out)
			}
		}
	}
	// Unset, the URL is the plugin's localhost default.
	local := pluginFiles(t, "export OBERTH_BASE_URL=\"https://LOCALHOST:30443/\"\n")
	if _, err := pluginHelper(t, local); err != nil {
		t.Fatalf("default URL and saved localhost: %v", err)
	}
	if out, err := pluginHelper(t, pluginFiles(t, "export OBERTH_BASE_URL=\""+saved+"\"\n")); err == nil {
		t.Fatalf("default URL received the token of %s: %q", saved, out)
	}
	if out, err := pluginHelper(t, pluginFiles(t, "")); err == nil {
		t.Fatalf("a token file naming no server was sent: %q", out)
	}
}

// Client files someone else can change are not read by the helper.
func TestClaudePluginHelperRefusesClientFilesOthersCanChange(t *testing.T) {
	for _, name := range []string{"", "env", "token"} {
		config := pluginFiles(t, "export OBERTH_BASE_URL=\"https://localhost:30443\"\n")
		target := filepath.Join(config, "oberth", name)
		info, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(target, info.Mode().Perm()|0o020); err != nil {
			t.Fatal(err)
		}
		if out, err := pluginHelper(t, config); err == nil || out != "" {
			t.Errorf("group-writable %s: %q %v", target, out, err)
		}
	}
	// A symbolic link to the directory is followed, as the CLI does: the
	// target's owner and mode decide.
	config := pluginFiles(t, "export OBERTH_BASE_URL=\"https://localhost:30443\"\n")
	linked := t.TempDir()
	if err := os.Symlink(filepath.Join(config, "oberth"), filepath.Join(linked, "oberth")); err != nil {
		t.Fatal(err)
	}
	if out, err := pluginHelper(t, linked); err != nil || !strings.Contains(out, "Bearer file-token") {
		t.Errorf("symbolic link to trusted client files: %q %v", out, err)
	}
	if err := os.Chmod(filepath.Join(config, "oberth"), 0o770); err != nil {
		t.Fatal(err)
	}
	if out, err := pluginHelper(t, linked); err == nil || out != "" {
		t.Errorf("symbolic link to a group-writable directory: %q %v", out, err)
	}
}
