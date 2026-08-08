package periapsis

import (
	"slices"
	"testing"
)

func TestTrivyFSUsesBakedOfflineDatabase(t *testing.T) {
	t.Parallel()
	step := (&ScanSDK{}).TrivyFS(".")
	position := slices.Index(step.Args, "--cache-dir")
	if position < 0 || position+1 >= len(step.Args) || step.Args[position+1] != "/seed/trivy" {
		t.Fatalf("Trivy args = %q; want baked cache directory", step.Args)
	}
	for _, flag := range []string{
		"--offline-scan", "--skip-db-update", "--skip-java-db-update", "--skip-check-update",
		"--skip-vex-repo-update", "--skip-version-check", "--disable-telemetry",
	} {
		if !slices.Contains(step.Args, flag) {
			t.Fatalf("Trivy args = %q; want %s", step.Args, flag)
		}
	}
	backend := slices.Index(step.Args, "--cache-backend")
	if backend < 0 || backend+1 >= len(step.Args) || step.Args[backend+1] != "memory" {
		t.Fatalf("Trivy args = %q; want memory-only cache backend", step.Args)
	}
}
