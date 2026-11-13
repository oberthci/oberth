package installer

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
)

// A fresh controller gets the default ceiling; an explicit flag (0 = no
// limit) and an existing release keep theirs.
func TestFreshArgoInstallSetsDefaultParallelism(t *testing.T) {
	t.Parallel()
	run := func(cfg Config, existing bool) []string {
		var install []string
		deps := Deps{KubeClient: argoSecurityTestClient(), Output: io.Discard, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			switch args[0] {
			case "list":
				if existing {
					return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
				}
				return []byte(`[]`), nil
			case "get":
				return []byte(`{}`), nil
			case "upgrade":
				install = slices.Clone(args)
			}
			return nil, nil
		}}
		if _, err := InstallArgoWorkflows(t.Context(), cfg, deps); err != nil {
			t.Fatal(err)
		}
		return install
	}
	if args := strings.Join(run(Config{}, false), " "); !strings.Contains(args, "controller.parallelism=4") || !strings.Contains(args, "controller.namespaceParallelism=0") {
		t.Fatalf("fresh install without the default ceiling: %s", args)
	}
	zero := 0
	if args := strings.Join(run(Config{ArgoParallelism: &zero}, false), " "); !strings.Contains(args, "controller.parallelism=0") || strings.Contains(args, "parallelism=4") {
		t.Fatalf("explicit 0 overridden: %s", args)
	}
	if args := strings.Join(run(Config{Upgrade: true}, true), " "); strings.Contains(args, "controller.parallelism") || strings.Contains(args, "controller.namespaceParallelism") {
		t.Fatalf("existing release got a default: %s", args)
	}
}
