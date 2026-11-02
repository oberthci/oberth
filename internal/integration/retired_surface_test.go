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

		"internal/gitcache/cache.go":         {"TagTime"},
		"internal/app/upstream_bootstrap.go": {"parseScannedHostKeys"},
		"internal/api/mcp_stream.go":         {"streamLongPoll"},
		"internal/gitcache/migrate_cache.go": {"CacheRoot", "ListFlatCaches"},
		"internal/gitcache/gc.go":            {"GC", "Compact", "gc"},
		"pkg/argoworkflow/source.go":         {"TriggerFiles"},
		"internal/sshserver/server.go":       {"HandleConn"},
		"internal/installer/upgrade.go":      {"UpgradePinnedKeys"},
		"cmd/oberth/secretstore_exec.go":     {"runSecretStoreMaterialize", "parseMaterializeArgs", "writeMaterializeSecret", "executeMaterialize"},
		"internal/app/artifacts.go":          {"ArtifactFailure", "reportArtifactFailure"},
		"internal/client/client.go":          {"GetTo"},
		"pkg/argoworkflow/declare.go":        {"AuthorizeSecretPaths"},
		"internal/store/store.go":            {"TrustedPlan", "TrustedApply"},
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
