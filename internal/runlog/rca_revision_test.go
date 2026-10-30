package runlog

import (
	"errors"
	"os"
	"testing"
)

func TestRCARevisionEmptyJournalInitializationNeverErasesEvidence(t *testing.T) {
	logs, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := logs.StepProgressHistory("run"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing evidence not unknown", err)
	}
	if err := logs.InitializeStepProgress("run"); err != nil {
		t.Fatal(err)
	}
	if events, err := logs.StepProgressHistory("run"); err != nil || len(events) != 0 {
		t.Fatal(events, err)
	}
	path, err := logs.progressPath("run")
	if err != nil {
		t.Fatal(err)
	}
	contents := []byte("torn earlier execution evidence")
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal(err)
	}
	if err := logs.InitializeStepProgress("run"); err != nil {
		t.Fatal(err)
	}
	if _, err := logs.StepProgressHistory("run"); err == nil {
		t.Fatal("corrupt evidence was erased")
	}
	actual, err := os.ReadFile(path)
	if err != nil || string(actual) != string(contents) {
		t.Fatal("journal truncated", string(actual), err)
	}
}
