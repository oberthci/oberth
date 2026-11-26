package model

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeIdentitySingleAccessor(t *testing.T) {
	for _, tree := range []string{"cmd", "internal", "pkg"} {
		scanNativeIdentity(t, tree)
	}
}

func scanNativeIdentity(t *testing.T, tree string) {
	t.Helper()
	err := filepath.WalkDir(filepath.Join("..", "..", tree), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skipNativeIdentity(path, d) {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if directIdentityRead(f, n) {
				t.Errorf("%s: direct WorkflowUID read", path)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func skipNativeIdentity(path string, d fs.DirEntry) bool {
	return d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.Contains(path, string(filepath.Separator)+"model"+string(filepath.Separator)) || strings.HasSuffix(path, filepath.FromSlash("store/run_mutations.go"))
}

func directIdentityRead(f *ast.File, n ast.Node) bool {
	s, ok := n.(*ast.SelectorExpr)
	if !ok || s.Sel.Name != "WorkflowUID" {
		return false
	}
	origin, isOrigin := s.X.(*ast.SelectorExpr)
	if !isOrigin || origin.Sel.Name != "Origin" {
		return false
	}
	return !identityAssignment(f, s)
}

func identityAssignment(f *ast.File, s *ast.SelectorExpr) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if a, ok := n.(*ast.AssignStmt); ok {
			for _, lhs := range a.Lhs {
				if lhs == s {
					found = true
				}
			}
		}
		return true
	})
	return found
}
