package service

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runprogress"
)

func TestStepProgressPodJournalKeepsControlsAndRecoverySeparate(t *testing.T) {
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "run.log"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	journal, err := runlog.OpenPodLogJournal(f, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	events := 0
	writer := newStepProgressLogWriter(newRunLogWriter(f, 1<<20), func(runprogress.Event) error { events++; return nil })
	writer.journal = journal
	now := time.Now().UTC()
	event := runprogress.Event{Version: runprogress.Version, Burn: "test", Step: "unit", Status: runprogress.StepRunning, StartedAt: &now}
	marker, err := runprogress.MarshalMarker(event)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(marker); err != nil {
		t.Fatal(err)
	}
	cursor := runlog.PodLogCursor{Key: "workflow/node/pod", WorkflowUID: "workflow", Node: "node", Pod: "pod", Container: "main", At: now, Count: 2, Bytes: 41}
	body := []byte("[test/unit] repeat\n[test/unit] repeat\n")
	if err := writer.CommitPodLog(cursor, body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	_ = journal.Close()
	recovered, err := runlog.OpenPodLogJournal(f, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	raw, _ := os.ReadFile(f.Name())
	if events != 1 || !bytes.Equal(raw, body) || recovered.Cursors()[cursor.Key].Count != 2 {
		t.Fatalf("controls/cursor/output changed: events=%d bytes=%q", events, raw)
	}
}
