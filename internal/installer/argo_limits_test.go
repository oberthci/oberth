package installer

import (
	"strings"
	"testing"
)

// Without limit flags the Argo install changes no limit, so --reuse-values
// keeps whatever an earlier install set.
func TestArgoHelmArgsSetNoLimitUnlessAsked(t *testing.T) {
	for _, arg := range ArgoHelmArgs(Config{}) {
		if strings.Contains(arg, "parallelism") || strings.Contains(arg, "extraObjects") {
			t.Fatalf("unrequested limit in %q", arg)
		}
	}
}

func TestValidateArgoLimitsRefusesAnUnmanagedController(t *testing.T) {
	one, negative := 1, -1
	if err := validateArgoLimits(Config{SkipArgo: true, ArgoParallelism: &one}); err == nil {
		t.Fatal("limits accepted with --skip-argo-workflows")
	}
	if err := validateArgoLimits(Config{ArgoParallelism: &negative}); err == nil {
		t.Fatal("negative parallelism accepted")
	}
}

func TestArgoHelmArgsRenderTheRequestedLimits(t *testing.T) {
	global, namespace := 9, 3
	args := strings.Join(ArgoHelmArgs(Config{ArgoParallelism: &global, ArgoNamespaceParallelism: &namespace}), " ")
	for _, want := range []string{"controller.parallelism=9", "controller.namespaceParallelism=3"} {
		if !strings.Contains(args, want) {
			t.Fatalf("missing native limit %s: %s", want, args)
		}
	}
}
