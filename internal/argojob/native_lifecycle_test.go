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

// Native retry/resubmission can erase execution history on which skip evidence relies.
func TestProductNeverRetriesOrResubmitsNativeWorkflows(t *testing.T) {
	t.Parallel()
	for _, tree := range []string{"cmd", "internal", "pkg"} {
		scanned := 0
		err := filepath.WalkDir(filepath.Join("..", "..", tree), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if strings.HasPrefix(entry.Name(), ".") || entry.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			scanned++
			return checkNativeLifecycleSource(t, path)
		})
		if err != nil {
			t.Fatal(err)
		}
		if scanned == 0 {
			t.Fatalf("no product source scanned in %s", tree)
		}
	}
}

func checkNativeLifecycleSource(t *testing.T, path string) error {
	t.Helper()
	positions := token.NewFileSet()
	file, err := parser.ParseFile(positions, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return err
	}
	ast.Inspect(file, func(node ast.Node) bool {
		id, ok := node.(*ast.Ident)
		if !ok {
			return true
		}
		switch id.Name {
		case "FormulateRetryWorkflow", "FormulateResubmitWorkflow", "RetryWorkflow", "ResubmitWorkflow", "RetryArchivedWorkflow", "ResubmitArchivedWorkflow":
			t.Errorf("%s: native retry/resubmission requires revisiting skip evidence: %s", positions.Position(id.Pos()), id.Name)
		}
		return true
	})
	return nil
}
