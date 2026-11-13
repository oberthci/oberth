package api

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestReadmeMCPToolsMatchRegistry(t *testing.T) {
	path := filepath.Join("..", "..", "README.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	start := strings.Index(source, "## MCP tools")
	if start < 0 {
		t.Fatal("missing MCP tool reference")
	}
	offset := strings.Index(source[start:], "Authenticated `GET`")
	if offset < 0 {
		t.Fatal("missing end of MCP tool reference")
	}
	end := start + offset
	var table strings.Builder
	fmt.Fprintf(&table, "## MCP tools\n\nThe full server registry contains **%d tools**.\nThe authenticated `tools/list` response is the authoritative registry.\nList it with `oberth mcp tools`. A non-admin token is not shown the admin-only\ntools (a call to one is still refused by the server); available operations are:\n\n| Tool | Description |\n|------|-------------|\n", len(toolDefinitions()))
	for _, tool := range toolDefinitions() {
		description := strings.ReplaceAll(tool["description"].(string), "\n", " ")
		description = strings.ReplaceAll(description, "|", "&#124;")
		table.WriteString("| `" + tool["name"].(string) + "` | " + description + " |\n")
	}
	expected := source[:start] + table.String() + "\n" + source[end:]
	if !updateToolReference(t, path, expected) && source != expected {
		t.Fatal("README reference drift: run OBERTH_UPDATE_TOOL_REFERENCE=1 go test ./internal/api -run 'Test(ReadmeMCPToolsMatchRegistry|MCPRegistryCountsMatchReferences)'")
	}
}

func TestMCPRegistryCountsMatchReferences(t *testing.T) {
	count := len(toolDefinitions())
	start := "<!-- BEGIN GENERATED MCP TOOL COUNT -->"
	end := "<!-- END GENERATED MCP TOOL COUNT -->"
	paragraph := fmt.Sprintf("%s\nThe full server registry contains **%d tools**, including administrator-only\noperations. The authenticated `tools/list` response is authoritative for the\nconnected identity; non-admin clients do not receive administrator-only tools.\n%s", start, count, end)
	for _, name := range []string{"docs/mcp-setup.md", "claude-plugin/skills/connect/SKILL.md", "claude-plugin/skills/oberth-agent-loop/SKILL.md", "AGENT-CONTRACT.md"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("..", "..", filepath.FromSlash(name))
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			pattern := regexp.MustCompile("(?s)" + regexp.QuoteMeta(start) + ".*?" + regexp.QuoteMeta(end))
			replacement := paragraph
			if name == "AGENT-CONTRACT.md" {
				pattern = regexp.MustCompile(`(?m)^- MCP exposes [0-9]+ tools:`)
				replacement = fmt.Sprintf("- MCP exposes %d tools:", count)
			}
			if len(pattern.FindAllIndex(data, -1)) != 1 {
				t.Fatal("expected one generated registry count region")
			}
			expected := pattern.ReplaceAllString(string(data), replacement)
			if !updateToolReference(t, path, expected) && expected != string(data) {
				t.Fatal("registry count drift: run OBERTH_UPDATE_TOOL_REFERENCE=1 go test ./internal/api -run 'Test(ReadmeMCPToolsMatchRegistry|MCPRegistryCountsMatchReferences)'")
			}
		})
	}
}

func updateToolReference(t *testing.T, path, expected string) bool {
	t.Helper()
	if os.Getenv("OBERTH_UPDATE_TOOL_REFERENCE") == "1" {
		if err := os.WriteFile(path, []byte(expected), 0644); err != nil {
			t.Fatal(err)
		}
		return true
	}
	return false
}
