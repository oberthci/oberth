package service

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/testdb"
)

var storeTemplate testdb.Template

func openTestStore(t *testing.T, ctx context.Context, path string, opts store.Options) (*store.Store, error) {
	t.Helper()
	storeTemplate.Copy(t, path, func(path string) error {
		database, err := store.Open(context.Background(), path, store.Options{})
		if err != nil {
			return err
		}
		return database.Close()
	})
	return store.Open(ctx, path, opts)
}
