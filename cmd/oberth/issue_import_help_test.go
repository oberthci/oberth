package main

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
)

func TestIssueImportHelpNamesFormatBoundsAndReference(t *testing.T) {
	app, out, diagnostic := fixture(t, fake.Bindings())
	if code := app.run(t.Context(), []string{"issue", "import", "--help"}); code != 0 {
		t.Fatal(code, diagnostic.String())
	}
	text := strings.Join(strings.Fields(out.String()), " ")
	for _, want := range []string{"JSON Lines", "1048576", "131072", "several files with disjoint numbers", "docs/issue-import.md", "in file order", "no issues", "before its first push"} {
		if !strings.Contains(text, want) {
			t.Fatal("missing import help", want, text)
		}
	}
}
