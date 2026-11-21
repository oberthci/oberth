// Package storetest copies closed store baselines into independent test files.
// Spec: docs/plans/c7-durable-state.md#owned-data-and-public-contract
package storetest

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Baseline owns one immutable closed database image for a package's tests.
type Baseline struct {
	mu    sync.Mutex
	build func(*testing.T, string)
	data  []byte
}

// New uses build once; build must close the database before returning.
func New(build func(*testing.T, string)) *Baseline {
	return &Baseline{build: build}
}

// Path returns a private writable copy owned and removed by t.
func (b *Baseline) Path(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	data := b.image(t, path)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func (b *Baseline) image(t *testing.T, path string) []byte {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.data == nil {
		b.build(t, path)
		checkpoint(t, path)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		b.data = data
	}
	return b.data
}

func checkpoint(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	var busy, pending, copied int
	if err := db.QueryRowContext(t.Context(), "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &pending, &copied); err != nil {
		t.Fatal(err)
	}
	if busy != 0 || pending != copied {
		t.Fatal("fixture database has uncheckpointed writes", busy, pending, copied)
	}
}
