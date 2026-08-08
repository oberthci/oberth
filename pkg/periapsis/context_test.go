package periapsis

import (
	"slices"
	"testing"
)

func TestTrivyFSUsesEphemeralWritableDatabase(t *testing.T) {
	t.Parallel()
	step := (&ScanSDK{}).TrivyFS(".")
	position := slices.Index(step.Args, "--cache-dir")
	if position < 0 || position+1 >= len(step.Args) || step.Args[position+1] != "/tmp/oberth-trivy" {
		t.Fatalf("Trivy args = %q; want writable ephemeral cache directory", step.Args)
	}
	for _, flag := range []string{
		"--offline-scan", "--skip-db-update", "--skip-java-db-update",
	} {
		if slices.Contains(step.Args, flag) {
			t.Fatalf("Trivy args = %q; baked-database flag %s is forbidden", step.Args, flag)
		}
	}
	for _, flag := range []string{"--scanners", "--skip-version-check", "--disable-telemetry"} {
		if !slices.Contains(step.Args, flag) {
			t.Fatalf("Trivy args = %q; want %s", step.Args, flag)
		}
	}
}
