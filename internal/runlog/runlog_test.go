package runlog

import (
	"errors"
	"os"
	"strings"
	"testing"
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
