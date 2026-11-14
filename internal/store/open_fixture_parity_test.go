package store_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/storetest"
	"github.com/oberthci/oberth/internal/store"
)

func TestFixtureOpenUsesProductionHeaderChecks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.sqlite")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	database, err := storetest.Open(t.Context(), path, store.Options{})
	if database != nil {
		database.Close()
	}
	if err == nil {
		t.Fatal("fixture opener initialized an existing empty file; production refuses it")
	}
}
