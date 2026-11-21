package main

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
)

// Every issue command that takes one issue takes its ID by position, as run
// commands do; no --id flag exists.
func TestIssueCommandsTakeTheIssueByPosition(t *testing.T) {
	for _, path := range []string{"issue get", "issue lock", "issue close", "issue reopen", "issue delete", "issue update", "issue append"} {
		t.Run(path, func(t *testing.T) {
			row, _, ok := findCommand(commands(), strings.Fields(path))
			if !ok {
				t.Fatal("missing command")
			}
			var extra []string
			if path == "issue append" {
				extra = []string{"--text", "note"}
			}
			data, problems := toolArgs(row, append([]string{"101"}, extra...), strings.NewReader(""), nil)
			if len(problems) != 0 || !strings.Contains(string(data), `"id":101`) {
				t.Fatal("positional issue ID refused", problems, string(data))
			}
			_, problems = toolArgs(row, append([]string{"--id", "101"}, extra...), strings.NewReader(""), nil)
			if strings.Join(problems, "\n") != "unknown flag --id; use the flags in command help\nmissing <id>" {
				t.Fatal("--id is still accepted", problems)
			}
			assertPositionalIssueHelp(t, path)
		})
	}
}

func assertPositionalIssueHelp(t *testing.T, path string) {
	t.Helper()
	if _, listed := commandCatalogue()[path].Flags["--id"]; listed {
		t.Fatal("catalogue lists --id")
	}
	app, out, diagnostic := fixture(t, fake.Bindings())
	if code := app.run(t.Context(), append(strings.Fields(path), "--help")); code != 0 {
		t.Fatal(code, diagnostic.String())
	}
	help := out.String()
	if !strings.HasPrefix(help, "oberth "+path+" <id> [flags]\n") || !strings.Contains(help, "\n<id> (integer, required): ") || strings.Contains(help, "--id") {
		t.Fatal("help does not show the positional ID", help)
	}
}
