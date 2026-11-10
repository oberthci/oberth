package installer

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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

// The headersHelper prints the header from OBERTH_TOKEN_COMMAND, else from
// the owner-only token file, and refuses a file others can read.
func TestClaudePluginHeadersHelper(t *testing.T) {
	helper, err := filepath.Abs(filepath.Join("..", "..", "claude-plugin", "scripts", "auth-header"))
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(helper); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("helper not executable: %v %v", info, err)
	}
	config := t.TempDir()
	run := func(environment ...string) (string, error) {
		command := exec.Command(helper)
		command.Env = append([]string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "XDG_CONFIG_HOME=" + config}, environment...)
		out, err := command.Output()
		return string(out), err
	}
	if _, err := run(); err == nil {
		t.Fatal("helper printed a header without a token")
	}
	if err := os.MkdirAll(filepath.Join(config, "oberth"), 0o700); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(config, "oberth", "token")
	if err := os.WriteFile(token, []byte("file-token\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(); err == nil {
		t.Fatal("helper read a token file others can read")
	}
	if err := os.Chmod(token, 0o600); err != nil {
		t.Fatal(err)
	}
	// A token that would need JSON escaping is refused, not printed.
	for _, unsafe := range []string{`printf 'a"b'`, `printf 'a\\b'`, `printf 'a b'`} {
		if out, err := run("OBERTH_TOKEN_COMMAND=" + unsafe); err == nil {
			t.Fatalf("%s printed %q", unsafe, out)
		}
	}
	for environment, want := range map[string]string{"": "file-token", "OBERTH_TOKEN_COMMAND=printf command-token": "command-token"} {
		var extra []string
		if environment != "" {
			extra = append(extra, environment)
		}
		out, err := run(extra...)
		var header map[string]string
		if err != nil || json.Unmarshal([]byte(out), &header) != nil || header["Authorization"] != "Bearer "+want {
			t.Fatalf("%q: %q %v", environment, out, err)
		}
	}
}
