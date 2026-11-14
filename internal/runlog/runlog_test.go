package runlog

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIndexAndReadStageOrStepSlices(t *testing.T) {
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
	stage, err := store.Read("r-1", "test", "")
	if err != nil || !strings.Contains(string(stage), "go test") || !strings.Contains(string(stage), "retained") || strings.Contains(string(stage), "go vet") {
		t.Fatalf("burn slice = %q, %v", stage, err)
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
	if index.Size != int64(len(body)) || len(index.Ranges) != 1 || index.Ranges[0].Stage != "test" || index.Ranges[0].Step != "unit" {
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

func TestResumeRetainsBytesWithoutProgressAuthority(t *testing.T) {
	logs, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := logs.Create("resumed")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("[test/unit] before\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	file, err = logs.Resume("resumed")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("[test/unit] after\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := logs.BuildIndex("resumed"); err != nil {
		t.Fatal(err)
	}
	body, err := logs.Read("resumed", "test", "unit")
	if err != nil || string(body) != "[test/unit] before\n[test/unit] after\n" {
		t.Fatalf("resumed bytes = %q %v", body, err)
	}
	file, err = logs.Resume("not-yet-written")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := logs.Resume("../escape"); err == nil {
		t.Fatal("resume escaped log root")
	}
}

func TestResumeIgnoresRetiredProgressFiles(t *testing.T) {
	directory := t.TempDir()
	logs, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "run.progress.jsonl"), []byte("malformed retired data\n"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := logs.Resume("run")
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
}
