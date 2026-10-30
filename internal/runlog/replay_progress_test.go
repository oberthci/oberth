package runlog

import (
	"os"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/runprogress"
)

func TestStepProgressHistoryRetainsStartsAndRejectsTornTail(t *testing.T) {
	logs, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	started := runprogress.Event{Version: 1, Burn: "publish", Step: "push", Ordinal: 0, Status: runprogress.StepRunning, DeclaredSize: "M", StartedAt: &now}
	queued := runprogress.Event{Version: 1, Burn: "publish", Step: "push", Ordinal: 0, Status: runprogress.StepQueued, DeclaredSize: "M"}
	for _, event := range []runprogress.Event{started, queued} {
		if err := logs.AppendStepProgress("history", event); err != nil {
			t.Fatal(err)
		}
	}
	display, err := logs.StepProgress("history")
	if err != nil || len(display) != 1 || display[0].Status != runprogress.StepQueued {
		t.Fatalf("display changed: %+v %v", display, err)
	}
	history, err := logs.StepProgressHistory("history")
	if err != nil || len(history) != 2 || history[0].StartedAt == nil {
		t.Fatalf("start erased by queued retry: %+v %v", history, err)
	}
	path, _ := logs.progressPath("history")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{\"version\":"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := logs.StepProgressHistory("history"); err == nil {
		t.Fatal("replay history ignored a torn record")
	}
	if display, err := logs.StepProgress("history"); err != nil || len(display) != 1 {
		t.Fatalf("display lost truncated-tail tolerance: %+v %v", display, err)
	}
}
