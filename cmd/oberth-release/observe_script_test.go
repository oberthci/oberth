package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The optional sampler's tests run on the branch's own files through Go,
// without adding a trusted Python entry to hack/check.sh.
func TestObservationScriptContract(t *testing.T) {
	dir := filepath.Join("..", "..", "hack")
	// Child-process reads must also invalidate the test cache.
	for _, name := range []string{"observe.py", "test_observe.py"} {
		if _, err := os.ReadFile(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(t.Context(), "python3", "-I", "-B", "-m", "unittest", "discover", "-s", dir, "-p", "test_observe.py")
	cmd.Env = append(os.Environ(), "TMPDIR="+t.TempDir())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("observation script: %v\n%s", err, out)
	}
}
