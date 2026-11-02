package integration_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// Retired APIs must stay absent: tests of their old behavior cannot justify
// retaining a second security or execution policy in the production binary.
func TestRetiredProductionSurface(t *testing.T) {
	cases := map[string][]string{
		"internal/store/store.go": {"TrustedPlan", "TrustedApply"},
	}
	for path, names := range cases {
		t.Run(path, func(t *testing.T) {
			f, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "..", path), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range f.Decls {
				if fn, ok := d.(*ast.FuncDecl); ok {
					for _, name := range names {
						if fn.Name.Name == name {
							t.Errorf("retired production API %s remains", name)
						}
					}
				}
			}
		})
	}
}
