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
		"internal/service/admission.go":         {"newWeightedAdmissionForJobs", "reserve", "reserveInOrder", "acquire"},
		"internal/service/api.go":               {"waitRun"},
		"internal/service/create_settlement.go": {"recordCreateAdmission", "pendingCreateCount"},
		"internal/service/promotion_retry.go":   {"waitBackground"},
		"internal/service/scheduler.go":         {"completePendingCancellations", "recoverPendingPublications"},
		"internal/service/secretstoreplan.go":   {"deriveStepPolicyName", "ExportComputeSecretStorePlan"},
		"internal/argojob/controller.go":        {"publish"},
		"internal/argojob/identitystore.go":     {"NewIdentityStore"},
		"internal/argojob/live_logs.go":         {"consume"},
		"internal/argojob/nonroot_terminal.go":  {"selectNonrootController"},
		"internal/argojob/resource_usage.go":    {"sampleResourceUsage"},

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

func TestRuntimeIdentitiesNeverUseKubernetesSecrets(t *testing.T) {
	for _, path := range []string{"internal/app/upstream_bootstrap.go", "cmd/oberth/admin.go", "cmd/oberth/serve.go"} {
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "..", path), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if s, ok := n.(*ast.SelectorExpr); ok && s.Sel.Name == "Secrets" {
				t.Errorf("%s still uses Kubernetes Secret identity storage", path)
			}
			return true
		})
	}
}
