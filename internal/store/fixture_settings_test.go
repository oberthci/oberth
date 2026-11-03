package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreFixturesUseDeclaredTempDir(t *testing.T) {
	dir := os.Getenv("OBERTH_TEST_STORE_TMPDIR")
	if dir == "" {
		t.Skip("no store fixture volume configured")
	}
	standard, err := os.MkdirTemp("", "store-fixture-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { requireStoreNoError(t, os.Remove(standard)) })
	for _, path := range []string{t.TempDir(), standard} {
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			t.Fatalf("fixture %q is outside declared directory %q: %v", path, dir, err)
		}
	}
}

func TestFixtureSyncSettingDoesNotChangeProductionConnections(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	var synchronous int
	if err := s.db.QueryRowContext(t.Context(), `PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if synchronous != 0 {
		t.Errorf("disposable fixture synchronous=%d, want OFF (0)", synchronous)
	}
	// Open the same file through the production entry point. The fixture's
	// connection setting must not persist in the file or affect other handles.
	production, err := OpenCurrent(t.Context(), s.livePath, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { requireStoreNoError(t, production.Close()) })
	if err := production.db.QueryRowContext(t.Context(), `PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if synchronous != 2 {
		t.Errorf("production synchronous=%d, want FULL (2)", synchronous)
	}
}
