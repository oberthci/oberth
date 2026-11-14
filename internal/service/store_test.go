package service

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
	"github.com/oberthci/oberth/internal/testdb"
)

var storeTemplate testdb.Template

func openTestStore(t *testing.T, ctx context.Context, path string, opts store.Options) (*store.Store, error) {
	t.Helper()
	storeTemplate.Copy(t, path, func(path string) error {
		database, err := storetest.Open(context.Background(), path, store.Options{})
		if err != nil {
			return err
		}
		return database.Close()
	})
	return storetest.Open(ctx, path, opts)
}

// putNativeStepFixture seeds a snapshot through the production batch writer.
func putNativeStepFixture(ctx context.Context, database *store.Store, result model.StepResult) (model.StepResult, error) {
	if err := database.PutStepResults(ctx, result.RunID, []model.StepResult{result}); err != nil {
		return model.StepResult{}, err
	}
	rows, err := database.StepResults(ctx, result.RunID)
	if err != nil {
		return model.StepResult{}, err
	}
	for _, row := range rows {
		if row.NodeID == result.NodeID {
			return row, nil
		}
	}
	return model.StepResult{}, store.ErrNotFound
}
