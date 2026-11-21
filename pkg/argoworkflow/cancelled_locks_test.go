package argoworkflow

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// cancelledLocksFixture is what lock status and run status are tested against:
// the Workflows the pinned controller leaves when a run holding locks is cancelled.
const cancelledLocksFixture = "../../internal/argojob/testdata/cancelled-locks.json"

func TestCancelledRunLockFixtureIsThePinnedControllerWrite(t *testing.T) {
	t.Parallel()
	directory := os.Getenv("OBERTH_CONTROLLER_OUTPUT")
	if directory == "" {
		directory = t.TempDir()
	}
	command := controllerFixture.Command(controllerContext(t), t, "-test.count=1", "-test.timeout=180s", "-test.run=^TestOberthCancelledRunLocks$", "-test.v")
	command.Env = append(os.Environ(), "OBERTH_CONTROLLER_CASES="+directory, "TMPDIR="+os.Getenv("GOTMPDIR"))
	if raw, err := controllerOutput(command); err != nil {
		t.Fatalf("cancelled run locks: %v\n%s", err, raw)
	}
	actual, err := os.ReadFile(filepath.Join(directory, "cancelled-locks.json"))
	if err != nil {
		t.Fatal(err)
	}
	expected, err := os.ReadFile(cancelledLocksFixture)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatal("cancelled run lock fixture differs from the pinned controller write", string(actual))
	}
}
