package service

import (
	"bytes"
	"errors"
	"io"
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

func TestStepProgressPodJournalFailureFallsBackOnce(t *testing.T) {
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "run.log"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	journal, err := runlog.OpenPodLogJournal(f, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	writer := newStepProgressLogWriter(newRunLogWriter(f, 1<<20), nil)
	writer.journal = journal
	writer.logFile = f
	// A failed journal descriptor models a runtime WAL error before projection.
	_ = journal.Close()
	c := runlog.PodLogCursor{Key: "workflow/node/pod", WorkflowUID: "workflow", Node: "node", Pod: "pod", Container: "main", At: time.Now().UTC(), Count: 1, Bytes: 20}
	body := []byte("[test/unit] real line\n")
	if err := writer.CommitPodLog(c, body); err != nil {
		t.Fatal("journal failure escaped", err)
	}
	c.Count++
	if err := writer.CommitPodLog(c, body); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(f.Name())
	if !writer.PodLogContinuityLost() || bytes.Count(raw, body) != 2 || bytes.Count(raw, []byte(runlog.PodLogContinuityLostMarker)) != 1 {
		t.Fatalf("fallback lost/duplicated bytes or marker: %q", raw)
	}
	if _, err := runlog.OpenPodLogJournal(f, 1<<20); err == nil {
		t.Fatal("degraded run trusted restart cursors again")
	}
}

type partialPodProjection struct {
	file  *os.File
	calls int
}

func (w *partialPodProjection) Write(b []byte) (int, error) {
	w.calls++
	n, err := w.file.Write(b[:min(5, len(b))])
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
func TestStepProgressPodProjectionFailureKeepsWALWithoutDuplicateAppend(t *testing.T) {
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "run.log"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	journal, err := runlog.OpenPodLogJournal(f, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	dest := &partialPodProjection{file: f}
	writer := newStepProgressLogWriter(dest, nil)
	writer.journal = journal
	writer.logFile = f
	c := runlog.PodLogCursor{Key: "workflow/node/pod", WorkflowUID: "workflow", Node: "node", Pod: "pod", Container: "main", At: time.Now().UTC(), Count: 1, Bytes: 20}
	body := []byte("[test/unit] exact received output\n")
	if err := writer.CommitPodLog(c, body); !errors.Is(err, runlog.ErrPodLogProjection) {
		t.Fatal("partial projection misclassified", err)
	}
	if err := writer.Close(); err == nil || dest.calls != 1 {
		t.Fatal("close replayed a partially-written line", dest.calls, err)
	}
	if writer.PodLogContinuityLost() {
		t.Fatal("valid recovery WAL was abandoned")
	}
	_ = journal.Close()
	recovered, err := runlog.OpenPodLogJournal(f, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	raw, _ := os.ReadFile(f.Name())
	if !bytes.Equal(raw, body) {
		t.Fatalf("partial projection was not recovered exactly: %q", raw)
	}
}

func TestStepProgressFailedRecoveryAppendsGapAfterRepairedPrefix(t *testing.T) {
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "run.log"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	j, err := runlog.OpenPodLogJournal(f, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	writer := newStepProgressLogWriter(&partialPodProjection{file: f}, nil)
	writer.journal = j
	writer.logFile = f
	c := runlog.PodLogCursor{Key: "workflow/node/pod", WorkflowUID: "workflow", Node: "node", Pod: "pod", Container: "main", At: time.Now().UTC(), Count: 1, Bytes: 20}
	body := []byte("[test/unit] received prefix must survive\n")
	if err := writer.CommitPodLog(c, body); !errors.Is(err, runlog.ErrPodLogProjection) {
		t.Fatal(err)
	}
	corrupt, err := os.OpenFile(f.Name()+".pods.jsonl", os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := corrupt.WriteString("{\"garbage\":1}\n"); err != nil {
		t.Fatal(err)
	}
	_ = corrupt.Close()
	writer.destination = newRunLogWriter(f, 1<<20)
	writer.recoverPodProjection(1 << 20)
	raw, _ := os.ReadFile(f.Name())
	if !bytes.HasPrefix(raw, body) || !bytes.Contains(raw, []byte(runlog.PodLogContinuityLostMarker)) || !writer.PodLogContinuityLost() {
		t.Fatalf("gap overwrote received prefix: %q", raw)
	}
}
