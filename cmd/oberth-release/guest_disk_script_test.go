package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The walk runs hack/vm-image/prepare.py from the admitted commit, so it is
// product code: its offline tests run here on the branch's own files, not as a
// trusted gate entry in hack/check.sh.
func TestGuestDiskScriptContract(t *testing.T) {
	root := filepath.Join("..", "..")
	dir := filepath.Join(root, "hack", "vm-image")
	// Child-process reads must also invalidate the test cache.
	for _, name := range []string{filepath.Join(dir, "prepare.py"), filepath.Join(dir, "test_prepare.py"), filepath.Join(root, ".oberth", "pins", "guest-disk.json")} {
		if _, err := os.ReadFile(name); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(t.Context(), "python3", "-I", "-B", "-m", "unittest", "discover", "-s", dir, "-p", "test_*.py")
	cmd.Env = append(os.Environ(), "TMPDIR="+t.TempDir())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("guest disk script: %v\n%s", err, out)
	}
}
