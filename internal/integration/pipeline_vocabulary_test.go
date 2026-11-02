package integration_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductionUsesArgoVocabulary(t *testing.T) {
	for _, root := range []string{"cmd", "internal", "pkg"} {
		err := filepath.WalkDir(filepath.Join("..", "..", root), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			for _, i := range f.Imports {
				if strings.Contains(i.Path.Value, "/pkg/periapsis") {
					t.Errorf("retired pipeline package import in %s", path)
				}
			}
			reported := false
			ast.Inspect(f, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && (id.Name == "JobRequest" || id.Name == "JobName" || id.Name == "CreateCI" || strings.Contains(strings.ToLower(id.Name), "burn")) {
					if !reported {
						t.Errorf("pre-Argo identifier %s in %s", id.Name, path)
						reported = true
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
