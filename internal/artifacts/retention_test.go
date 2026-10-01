package artifacts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func storeRun(t *testing.T, store *Store, runID, body string, age time.Duration) {
	t.Helper()
	if _, err := store.Extract(runID, archiveOf(t, member{name: "report.txt", body: body}), 1<<20, DefaultScanPatterns); err != nil {
		t.Fatalf("seed %s: %v", runID, err)
	}
	stamp := time.Now().Add(-age)
	path := filepath.Join(store.directory, runID)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

func runsPresent(t *testing.T, store *Store) []string {
	t.Helper()
	entries, err := os.ReadDir(store.directory)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names
}

func TestEvictRemovesOldestRunsUntilUnderBudget(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	storeRun(t, store, "run-oldest", strings.Repeat("a", 400), 3*time.Hour)
	storeRun(t, store, "run-middle", strings.Repeat("b", 400), 2*time.Hour)
	storeRun(t, store, "run-newest", strings.Repeat("c", 400), 1*time.Hour)

	removed, err := store.Evict(500)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("evicted %v, want the two oldest", removed)
	}
	present := runsPresent(t, store)
	if len(present) != 1 || present[0] != "run-newest" {
		t.Fatalf("remaining runs %v, want only run-newest", present)
	}
}

func TestEvictKeepsEverythingWhenUnderBudget(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	storeRun(t, store, "run-a", "small", time.Hour)
	storeRun(t, store, "run-b", "small", time.Minute)

	removed, err := store.Evict(1 << 20)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("evicted %v while under budget", removed)
	}
	if len(runsPresent(t, store)) != 2 {
		t.Fatal("a run disappeared while under budget")
	}
}

func TestEvictNeverTouchesAnythingOutsideItsOwnDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	logs := filepath.Join(root, "logs")
	if err := os.MkdirAll(logs, 0o750); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(logs, "run-oldest.log")
	if err := os.WriteFile(logFile, []byte("the run log"), 0o640); err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	storeRun(t, store, "run-oldest", strings.Repeat("a", 4000), 2*time.Hour)

	if _, err := store.Evict(10); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if len(runsPresent(t, store)) != 0 {
		t.Fatal("the oversized run was not evicted")
	}
	body, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("eviction removed the run log: %v", err)
	}
	if string(body) != "the run log" {
		t.Fatal("eviction altered the run log")
	}
}

func TestEvictWithNoBudgetKeepsEverything(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	storeRun(t, store, "run-a", strings.Repeat("a", 4000), time.Hour)
	removed, err := store.Evict(0)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("an unset budget evicted %v", removed)
	}
}

func TestUsageReportsTheTotalStoredBytes(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	storeRun(t, store, "run-a", strings.Repeat("a", 100), time.Hour)
	storeRun(t, store, "run-b", strings.Repeat("b", 200), time.Minute)

	total, err := store.Usage()
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if total != 300 {
		t.Fatalf("Usage = %d, want 300", total)
	}
}
