package argojob

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The run-slot reason matches the pinned controller's own message: the
// literal in its throttling path, read from the module this build pins.
func TestThrottledMessageIsThePinnedControllersOwn(t *testing.T) {
	t.Parallel()
	out, err := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Dir}}", "github.com/argoproj/argo-workflows/v4").Output()
	if err != nil {
		t.Fatal("pinned Argo module not found", err)
	}
	source, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(out)), "workflow", "controller", "controller.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "markWorkflowPhase(ctx, wfv1.WorkflowPending, "+strconv.Quote(throttledMessage)+")") {
		t.Fatal("the pinned controller no longer postpones a Workflow with this message", throttledMessage)
	}
}
