package main

import (
	"slices"
	"strings"
	"testing"
)

func TestProductionRootsAreExplicitExistingMains(t *testing.T) {
	for _, tc := range []struct {
		name, file, pkg string
		listed, harness bool
		root, finding   bool
	}{
		{"release without server", "cmd/oberth-release/main.go", "main", true, false, true, false},
		{"source without server", "cmd/oberth-source-plugin/main.go", "main", true, false, true, false},
		{"unlisted command", "cmd/extra/main.go", "main", false, false, false, true},
		{"main directly under cmd", "cmd/main.go", "main", false, false, false, true},
		{"declared harness", "cmd/fixture/main.go", "main", false, true, false, false},
		{"listed harness", "cmd/fixture/main.go", "main", true, true, true, false},
		{"test main", "cmd/fixture/main_test.go", "main", true, false, false, false},
		{"unlisted test main", "cmd/fixture/main_test.go", "main", false, false, false, false},
		{"library is not a command", "cmd/library/library.go", "library", true, false, false, false},
		{"main outside commands", "component/a/main.go", "main", false, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture(t)
			p.Components["C1"] = append(p.Components["C1"], "cmd/**")
			p.ProductionCommands = []string{"cmd/absent"}
			dir := tc.file[:strings.LastIndex(tc.file, "/")]
			if tc.listed {
				p.ProductionCommands = append(p.ProductionCommands, dir)
			}
			if tc.harness {
				p.Harness.Trees = []string{dir + "/**"}
			}
			code := "// Command fixture follows its contract.\n// Spec: docs/plans/contract.md#behavior\npackage " + tc.pkg + "\n"
			hits := scan(t, p, map[string]string{tc.file: code})
			if tc.finding {
				require(t, hits, "DEP-ROOT")
			} else {
				clean(t, hits)
			}
			c := inspect("example.invalid/unit", []byte(tc.file+"\x00"))
			if got := slices.Contains(c.roots, "example.invalid/unit/"+dir); got != tc.root {
				t.Fatalf("root = %v, want %v: %v", got, tc.root, c.roots)
			}
			if slices.Contains(c.roots, "example.invalid/unit/cmd/absent") {
				t.Fatal("missing command became a root")
			}
		})
	}
}

func TestUnlistedMainIsReportedOncePerCommand(t *testing.T) {
	p := fixture(t)
	p.Components["C1"] = append(p.Components["C1"], "cmd/**")
	code := "// Command fixture follows its contract.\n// Spec: docs/plans/contract.md#behavior\npackage main\n"
	hits := scan(t, p, map[string]string{
		"cmd/extra/main.go":      code + "func main() {}\n",
		"cmd/extra/other.go":     code + "const Other = 1\n",
		"cmd/extra/main_test.go": "package main_test\n",
	})
	count := 0
	for _, hit := range hits {
		if strings.Contains(hit, "[DEP-ROOT]") {
			count++
		}
	}
	if count != 1 {
		t.Fatal(hits)
	}
}

func TestNestedFencesKeepRealHeadingsVisible(t *testing.T) {
	p := fixture(t)
	write(t, "docs/plans/nested.md", "~~~\n```\n## Hidden\n~~~\n## Visible\n")
	hits := scan(t, p, map[string]string{"component/a/a.go": "// Spec: docs/plans/nested.md#visible\npackage a\n"})
	clean(t, hits)
	c := inspect("example.invalid/unit", []byte("component/a/a.go\x00"))
	if c.validSpec("docs/plans/nested.md#hidden") || !c.validSpec("docs/plans/nested.md#visible") {
		t.Fatal("nested fence obscured the real heading")
	}
}
