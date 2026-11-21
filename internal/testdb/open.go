// Package testdb creates isolated current-schema databases for tests.
// Spec: docs/plans/c7-durable-state.md#owned-data-and-public-contract
package testdb

import (
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/store"
)

func Open(t *testing.T, options store.Options) *store.Store {
	t.Helper()
	state, err := store.Create(t.Context(), filepath.Join(t.TempDir(), "state.sqlite"), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := state.Close(); err != nil {
			t.Error(err)
		}
	})
	return state
}
