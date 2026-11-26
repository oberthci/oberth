package argoworkflow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/argoworkflow/controllertest"
)

func TestPinnedSemaphoreWaiterWakes(t *testing.T) {
	t.Parallel()
	testPinnedSync(t, "TestOberthSemaphoreWaiterWakes")
}

func TestPinnedFailedWorkflowReleasesEveryPermit(t *testing.T) {
	t.Parallel()
	testPinnedSync(t, "TestOberthFailedWorkflowReleasesEveryPermit")
}

func testPinnedSync(t *testing.T, test string) {
	t.Helper()
	ctx := controllerContext(t)
	module := controllertest.Module(ctx, t)
	entries, err := os.ReadDir(filepath.Join(module, "workflow/sync"))
	if err != nil {
		t.Fatal(err)
	}
	// Compile only the pinned sync package and this fixture in a private module.
	// No upstream tests, controller, database or cluster is started.
	directory := t.TempDir()
	copyFile := func(source, name string) {
		t.Helper()
		raw, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			copyFile(filepath.Join(module, "workflow/sync", name), name)
		}
	}
	for _, name := range []string{"go.mod", "go.sum"} {
		copyFile(filepath.Join("../..", name), name)
	}
	copyFile("testdata/semaphore_wakeup_test.go", "oberth_wakeup_test.go")
	copyFile("testdata/failed_workflow_locks_test.go", "oberth_failed_locks_test.go")
	command := exec.CommandContext(ctx, "go", "test", "-race", "-mod=readonly", "-p=1", "-buildvcs=false",
		"-count=1", "-timeout=120s", "-run=^"+test+"$", "-v", ".")
	command.Dir = directory
	if raw, err := controllerOutput(command); err != nil {
		t.Fatalf("pinned semaphore reproduction: %v\n%s", err, raw)
	}
}
