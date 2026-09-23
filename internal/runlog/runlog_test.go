package runlog

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestIndexAndReadBurnOrStepSlices(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.Create("r-1")
	if err != nil {
		t.Fatal(err)
	}
	body := "[lint/vet] $ go vet ./...\n[lint/vet] passed\n[test/unit] $ go test ./...\n[test/unit] failed\n[test/summary] retained\n"
	if _, err := file.WriteString(body); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	index, err := store.BuildIndex("r-1")
	if err != nil {
		t.Fatal(err)
	}
	if index.Size != int64(len(body)) || len(index.Ranges) != 3 {
		t.Fatalf("index = %#v", index)
	}
	burn, err := store.Read("r-1", "test", "")
	if err != nil || !strings.Contains(string(burn), "go test") || !strings.Contains(string(burn), "retained") || strings.Contains(string(burn), "go vet") {
		t.Fatalf("burn slice = %q, %v", burn, err)
	}
	unit, err := store.Read("r-1", "test", "unit")
	if err != nil || !strings.Contains(string(unit), "go test") || strings.Contains(string(unit), "retained") {
		t.Fatalf("unit slice = %q, %v", unit, err)
	}
	summary, err := store.Read("r-1", "test", "summary")
	if err != nil || string(summary) != "[test/summary] retained\n" {
		t.Fatalf("summary slice = %q, %v", summary, err)
	}
}

func TestReadRejectsOversizedSliceAndIndexCoalescesLines(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.Create("r-bounded")
	if err != nil {
		t.Fatal(err)
	}
	line := "[test/unit] " + strings.Repeat("x", 1024) + "\n"
	for written := 0; written <= maxReadBytes; written += len(line) {
		if _, err := file.WriteString(line); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	index, err := store.BuildIndex("r-bounded")
	if err != nil {
		t.Fatal(err)
	}
	if len(index.Ranges) != 1 {
		t.Fatalf("ranges = %d, want one contiguous range", len(index.Ranges))
	}
	if _, err := store.Read("r-bounded", "test", "unit"); !errors.Is(err, ErrLogSliceTooLarge) {
		t.Fatalf("Read error = %v, want ErrLogSliceTooLarge", err)
	}
}

func TestCreateIsExclusiveAndPathsRejectTraversal(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.Create("r-1")
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if _, err := store.Create("r-1"); err == nil {
		t.Fatal("duplicate log creation succeeded")
	}
	if _, err := store.Create("../escape"); err == nil {
		t.Fatal("traversal run ID succeeded")
	}
	if _, err := os.Stat(store.directory + "/../escape.log"); !os.IsNotExist(err) {
		t.Fatalf("escape path exists: %v", err)
	}
}

func TestLongLineDoesNotRepaintPassingRunAsIndexFailure(t *testing.T) {
	store, _ := Open(t.TempDir())
	file, _ := store.Create("r-long")
	body := "[test/unit] " + strings.Repeat("x", 5<<20)
	_, _ = file.WriteString(body)
	_ = file.Close()
	index, err := store.BuildIndex("r-long")
	if err != nil {
		t.Fatal(err)
	}
	if index.Size != int64(len(body)) || len(index.Ranges) != 1 || index.Ranges[0].Burn != "test" || index.Ranges[0].Step != "unit" {
		t.Fatalf("long-line index = %#v", index)
	}
	if _, err := store.Read("r-long", "test", "unit"); !errors.Is(err, ErrLogSliceTooLarge) {
		t.Fatalf("long-line read error = %v, want bounded response error", err)
	}
}

// TestBuildIndexOnReconciledRunProducesReadableIndex verifies that a log
// file from a reconciled run (partial content, possibly no terminal marker)
// still produces a valid index whose ranges are readable. Reconciled runs
// may have incomplete logs because the scheduler was not observing the Job
// when it completed (#437).
func TestBuildIndexOnReconciledRunProducesReadableIndex(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.Create("r-reconciled")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a reconciled run: the log has a complete first step and an
	// abruptly-ended second step (no closing line, simulating a crash).
	body := "[lint/vet] $ go vet ./...\n[lint/vet] ok\n[test/unit] $ go test ./...\n[test/unit] PASS some/pkg\n"
	if _, err := file.WriteString(body); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	index, err := store.BuildIndex("r-reconciled")
	if err != nil {
		t.Fatalf("BuildIndex on reconciled run: %v", err)
	}
	if index.Size != int64(len(body)) {
		t.Fatalf("index.Size = %d, want %d", index.Size, len(body))
	}
	if len(index.Ranges) != 2 {
		t.Fatalf("index.Ranges = %d, want 2 (lint/vet + test/unit)", len(index.Ranges))
	}
	// Both steps must be independently readable.
	vetSlice, err := store.Read("r-reconciled", "lint", "vet")
	if err != nil {
		t.Fatalf("Read lint/vet: %v", err)
	}
	if !strings.Contains(string(vetSlice), "go vet") {
		t.Fatalf("lint/vet slice = %q, want to contain 'go vet'", vetSlice)
	}
	unitSlice, err := store.Read("r-reconciled", "test", "unit")
	if err != nil {
		t.Fatalf("Read test/unit: %v", err)
	}
	if !strings.Contains(string(unitSlice), "go test") {
		t.Fatalf("test/unit slice = %q, want to contain 'go test'", unitSlice)
	}
}

func TestReadFromServesGrowingLogWithTailAndCursor(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Absent log (queued run): callers see os.ErrNotExist and decide.
	if _, _, _, err := store.ReadFrom("r-live", -1, 64); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing log error = %v", err)
	}
	file, err := store.Create("r-live")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	write := func(text string) {
		t.Helper()
		if _, err := file.WriteString(text); err != nil {
			t.Fatal(err)
		}
	}
	write("[setup/go] one\n[setup/go] two\n")
	chunk, next, size, err := store.ReadFrom("r-live", -1, 1<<20)
	if err != nil || string(chunk) != "[setup/go] one\n[setup/go] two\n" || next != size || size != 30 {
		t.Fatalf("tail read = %q next=%d size=%d err=%v", chunk, next, size, err)
	}
	// Nothing new: empty chunk, cursor unchanged.
	chunk, next2, _, err := store.ReadFrom("r-live", next, 1<<20)
	if err != nil || len(chunk) != 0 || next2 != next {
		t.Fatalf("idle read = %q next=%d err=%v", chunk, next2, err)
	}
	// The file grew: only the appended bytes come back.
	write("[lint/vet] three\n")
	chunk, next3, _, err := store.ReadFrom("r-live", next2, 1<<20)
	if err != nil || string(chunk) != "[lint/vet] three\n" || next3 != next2+17 {
		t.Fatalf("grow read = %q next=%d err=%v", chunk, next3, err)
	}
	// A bounded mid-stream read is trimmed to the last complete line.
	chunk, next4, _, err := store.ReadFrom("r-live", 0, 20)
	if err != nil || string(chunk) != "[setup/go] one\n" || next4 != 15 {
		t.Fatalf("trimmed read = %q next=%d err=%v", chunk, next4, err)
	}
	// A bound smaller than the first line still makes progress untrimmed.
	chunk, next5, _, err := store.ReadFrom("r-live", 0, 4)
	if err != nil || string(chunk) != "[set" || next5 != 4 {
		t.Fatalf("progress read = %q next=%d err=%v", chunk, next5, err)
	}
	// Tail mode with a small bound starts near the end, not at zero.
	chunk, _, size, err = store.ReadFrom("r-live", -1, 10)
	if err != nil || size != 47 || len(chunk) == 0 || len(chunk) > 10 {
		t.Fatalf("bounded tail = %q size=%d err=%v", chunk, size, err)
	}
	// An offset beyond the file clamps cleanly.
	chunk, next6, _, err := store.ReadFrom("r-live", 9000, 64)
	if err != nil || len(chunk) != 0 || next6 != 47 {
		t.Fatalf("clamped read = %q next=%d err=%v", chunk, next6, err)
	}
	// Bounds and identifiers stay validated.
	if _, _, _, err := store.ReadFrom("r-live", 0, 0); err == nil {
		t.Fatal("zero byte bound was accepted")
	}
	if _, _, _, err := store.ReadFrom("../escape", 0, 64); err == nil {
		t.Fatal("path traversal was accepted")
	}
}

func TestStepProgressJournalReducesTransitionsInPlanOrder(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	queued := runprogress.Event{
		Version: runprogress.Version, Burn: "test", Step: "unit", Ordinal: 0,
		Status: runprogress.StepQueued, DeclaredSize: periapsis.L,
	}
	if err := store.AppendStepProgress("r-progress", queued); err != nil {
		t.Fatal(err)
	}
	second := queued
	second.Step, second.Ordinal = "race", 1
	if err := store.AppendStepProgress("r-progress", second); err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC()
	running := queued
	running.Status, running.StartedAt = runprogress.StepRunning, &started
	if err := store.AppendStepProgress("r-progress", running); err != nil {
		t.Fatal(err)
	}

	events, err := store.StepProgress("r-progress")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Step != "unit" || events[0].Status != runprogress.StepRunning ||
		events[1].Step != "race" || events[1].Status != runprogress.StepQueued {
		t.Fatalf("progress snapshot = %#v", events)
	}
	// A crash may leave one non-newline-terminated tail. Earlier complete
	// records remain readable and authoritative.
	file, err := os.OpenFile(filepath.Join(store.directory, "r-progress.progress.jsonl"), os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"version":`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	events, err = store.StepProgress("r-progress")
	if err != nil || len(events) != 2 || events[0].Status != runprogress.StepRunning {
		t.Fatalf("progress after truncated tail = %#v, %v", events, err)
	}
	second.Status, second.StartedAt = runprogress.StepRunning, &started
	if err := store.AppendStepProgress("r-progress", second); err != nil {
		t.Fatal(err)
	}
	events, err = store.StepProgress("r-progress")
	if err != nil || len(events) != 2 || events[1].Status != runprogress.StepRunning {
		t.Fatalf("progress after repaired tail = %#v, %v", events, err)
	}
	if err := store.AppendStepProgress("../escape", queued); err == nil {
		t.Fatal("progress journal accepted a traversal run ID")
	}
}

func TestReadActiveReturnsOnlyCurrentStepWithoutWritingTerminalIndex(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.Create("r-active-step")
	if err != nil {
		t.Fatal(err)
	}
	body := "[setup/prepare] unrelated before\n" +
		"[release/publish] $ publish alias\n" +
		"[release/publish] first current line\n" +
		"[verify/check] unrelated middle\n" +
		"[release/publish] concurrent service line\n" +
		"[verify/check] unrelated after\n"
	if _, err := file.WriteString(body); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	current, err := store.ReadActive("r-active-step", "release", "publish")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(current), "first current line") || !strings.Contains(string(current), "concurrent service line") ||
		strings.Contains(string(current), "unrelated") {
		t.Fatalf("active step log = %q", current)
	}
	if _, err := os.Stat(filepath.Join(store.directory, "r-active-step.index.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active read wrote terminal index: %v", err)
	}
}
