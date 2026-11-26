package argojob_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// Native retry/resubmission can erase execution history on which skip evidence
// relies. The one exception is the resume planner's retry of a failed
// Workflow, whose successor is Assisted from its insert: exactly one call.
func TestNativeRetrySingleCallSite(t *testing.T) {
	t.Parallel()
	calls := map[string]int{}
	for _, tree := range []string{"cmd", "internal", "pkg"} {
		if scanned := scanNativeLifecycle(t, tree, calls); scanned == 0 {
			t.Fatalf("no product source scanned in %s", tree)
		}
	}
	if len(calls) != 1 || calls[filepath.Join("..", "..", "internal", "argojob", "resume.go")+":formulateRetry"] != 1 {
		t.Errorf("native retry must have exactly one call site, formulateRetry in resume.go: %v", calls)
	}
}

func scanNativeLifecycle(t *testing.T, tree string, calls map[string]int) int {
	t.Helper()
	scanned := 0
	err := filepath.WalkDir(filepath.Join("..", "..", tree), func(path string, entry fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case entry.IsDir() && (strings.HasPrefix(entry.Name(), ".") || entry.Name() == "testdata"):
			return filepath.SkipDir
		case entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}
		scanned++
		return checkNativeLifecycleSource(t, path, calls)
	})
	if err != nil {
		t.Fatal(err)
	}
	return scanned
}

func checkNativeLifecycleSource(t *testing.T, path string, calls map[string]int) error {
	t.Helper()
	positions := token.NewFileSet()
	file, err := parser.ParseFile(positions, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return err
	}
	for _, declaration := range file.Decls {
		function := ""
		if f, ok := declaration.(*ast.FuncDecl); ok {
			function = f.Name.Name
		}
		ast.Inspect(declaration, func(node ast.Node) bool {
			id, ok := node.(*ast.Ident)
			if !ok {
				return true
			}
			switch id.Name {
			case "FormulateRetryWorkflow":
				calls[path+":"+function]++
			case "FormulateResubmitWorkflow", "RetryWorkflow", "ResubmitWorkflow", "RetryArchivedWorkflow", "ResubmitArchivedWorkflow":
				t.Errorf("%s: native retry/resubmission requires revisiting skip evidence: %s", positions.Position(id.Pos()), id.Name)
			}
			return true
		})
	}
	return nil
}
