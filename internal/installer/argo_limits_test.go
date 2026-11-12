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
