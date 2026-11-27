// Package agent_test exercises the instruction and verified fetch contracts.
// Spec: docs/plans/c12-agent-client.md#inspections
package agent_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Covers: REQ-C12-001
func TestAgentRegistrationContract(t *testing.T) {
	runAgentContracts(t, "test_guidance")
}

// Covers: REQ-C12-003
func TestAtomicCliInstallationContract(t *testing.T) {
	runAgentContracts(t, "test_fetch_install")
}

func TestAgentClientContract(t *testing.T) {
	runAgentContracts(t, "test_fetch", "test_fetch_support", "test_portability")
}

func TestRunBoxContract(t *testing.T) {
	runAgentContracts(t, "test_run_box")
}

func runAgentContracts(t *testing.T, modules ...string) {
	t.Helper()
	trackInputs(t)
	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "oberth")
	build := exec.CommandContext(ctx, "go", "build", "-p=1", "-o", binary, "./cmd/oberth")
	build.Dir = ".."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build real command table: %v\n%s", err, out)
	}
	args := append([]string{"-m", "unittest", "-v"}, modules...)
	cmd := exec.CommandContext(ctx, "python3", args...)
	cmd.Env = append(os.Environ(), "TMPDIR="+t.TempDir(), "OBERTH_CLI="+binary)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("agent contracts: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if summary := lines[len(lines)-1]; strings.Contains(summary, "skipped=") {
		t.Log(summary)
	}
}

func trackInputs(t *testing.T) {
	t.Helper()
	// Child-process file reads must also invalidate the test cache.
	for _, root := range []string{".", "../cmd/oberth", "../internal/client", "../internal/apicontract", "../internal/api/rpc.go", "../internal/api/skills", "../internal/app/resume.go", "../internal/nativebinding/resume.go", "../internal/admissioncontract/plan.go", "../internal/app/messages.go", "../internal/stepmodel", "../internal/releaseverify/origin.go", "../hack/release-origin.py", "../internal/workflowpolicy/release-version.pattern"} {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "__pycache__" {
					return filepath.SkipDir
				}
				return nil
			}
			_, err = os.ReadFile(path)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Include the owner pin, including its unprovisioned contents, in the cache.
	if _, err := os.ReadFile("../internal/releaseverify/release-cosign.pub"); err != nil {
		t.Fatal(err)
	}
}
