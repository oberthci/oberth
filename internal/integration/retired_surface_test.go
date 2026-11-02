package integration_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

// Retired APIs must stay absent: tests of their old behavior cannot justify
// retaining a second security or execution policy in the production binary.
func TestRetiredProductionSurface(t *testing.T) {
	cases := map[string][]string{
		"internal/app/artifacts.go":   {"ArtifactFailure", "reportArtifactFailure"},
		"internal/client/client.go":   {"GetTo"},
		"pkg/argoworkflow/declare.go": {"AuthorizeSecretPaths"},
		"internal/store/store.go":     {"TrustedPlan", "TrustedApply"},
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

func TestRetiredFilesAbsent(t *testing.T) {
	for _, path := range []string{"pkg/argoworkflow/nonblocking.go", "internal/integration/testdata/gitssh/main.go"} {
		if _, err := os.Stat(filepath.Join("..", "..", path)); !os.IsNotExist(err) {
			t.Errorf("retired file %s must be absent (stat: %v)", path, err)
		}
	}
}
