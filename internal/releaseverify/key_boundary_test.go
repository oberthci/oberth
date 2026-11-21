package releaseverify

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func TestFixtureKeyEntryHasOnlyTestCallers(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".cache", "node_modules", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		return checkPinReferences(t, path, filepath.ToSlash(rel))
	})
	if err != nil {
		t.Fatal(err)
	}
}
func checkPinReferences(t *testing.T, path, rel string) error {
	t.Helper()
	if strings.HasPrefix(rel, "internal/releaseverify/verifytest/") {
		return nil
	}
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		return err
	}
	ast.Inspect(file, func(node ast.Node) bool {
		id, ok := node.(*ast.Ident)
		if ok && id.Name == "RunTestsWithKey" {
			t.Errorf("product source references test key injection: %s", rel)
		}
		return true
	})
	return nil
}

func TestFixtureKeyEntryRefusesProductExecution(t *testing.T) {
	positions := token.NewFileSet()
	file, err := parser.ParseFile(positions, "verifytest/runner.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range file.Decls {
		fn, ok := node.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "WithKey" {
			continue
		}
		if fn.Body == nil || len(fn.Body.List) == 0 {
			t.Fatal("test key entry lacks a product refusal")
		}
		var first bytes.Buffer
		if err := format.Node(&first, positions, fn.Body.List[0]); err != nil {
			t.Fatal(err)
		}
		// This exact first statement must refuse before any key access or mutation.
		if first.String() != "if !testing.Testing() {\n\treturn 1\n}" {
			t.Fatal("test key injection must refuse product execution before key access")
		}
		return
	}
	t.Fatal("test key entry is missing its product refusal")
}
