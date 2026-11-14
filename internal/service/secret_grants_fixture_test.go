package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/store"
)

func testAccessStore(t *testing.T, now *time.Time) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oberth.db")
	database, err := openTestStore(t, context.Background(), path, store.Options{
		Now: func() time.Time { return *now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return database
}
