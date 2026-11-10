package api

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	if os.Getenv("OBERTH_UPDATE_TOOL_REFERENCE") == "1" {
		var table strings.Builder
		table.WriteString("## MCP tools\n\nThe authenticated `tools/list` response is the authoritative registry.\nList it with `oberth mcp tools`. A non-admin token is not shown the admin-only\ntools (a call to one is still refused by the server); available operations are:\n\n| Tool | Description |\n|------|-------------|\n")
		for _, tool := range toolDefinitions() {
			description := strings.ReplaceAll(tool["description"].(string), "\n", " ")
			description = strings.ReplaceAll(description, "|", "&#124;")
			table.WriteString("| `" + tool["name"].(string) + "` | " + description + " |\n")
		}
		source = source[:start] + table.String() + "\n" + source[end:]
		if err := os.WriteFile(path, []byte(source), 0644); err != nil {
			t.Fatal(err)
		}
		end = strings.Index(source[start:], "Authenticated `GET`") + start
	}
	var documented []string
	for _, match := range regexp.MustCompile("(?m)^\\| `([^`]+)` ").FindAllStringSubmatch(source[start:end], -1) {
		documented = append(documented, match[1])
	}
	actual := ToolNames()
	slices.Sort(actual)
	slices.Sort(documented)
	if !slices.Equal(documented, actual) {
		t.Fatalf("README tools differ from registry: documented %d %v; actual %d %v", len(documented), documented, len(actual), actual)
	}
}
