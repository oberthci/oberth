package storetest

import (
	"context"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type runStarter interface {
	StartRun(context.Context, string) (model.Run, error)
	Run(context.Context, string) (model.Run, error)
}

// NextQueuedRun selects historical setup data; production never dispatches rows.
func NextQueuedRun(database runStarter, ctx context.Context) (model.Run, error) {
	history, ok := database.(interface {
		ListRecentRuns(context.Context, model.RunListFilter) ([]model.Run, error)
	})
	if !ok {
		return model.Run{}, store.ErrNotFound
	}
	runs, err := history.ListRecentRuns(ctx, model.RunListFilter{Limit: 500})
	if err != nil {
		return model.Run{}, err
	}
	var oldest model.Run
	for _, r := range runs {
		if r.Status != model.RunQueued {
			continue
		}
		if oldest.ID == "" || r.QueueSequence < oldest.QueueSequence {
			oldest = r
		}
	}
	if oldest.ID == "" {
		return model.Run{}, store.ErrNotFound
	}
	return oldest, nil
}
func ClaimNextRun(database runStarter, ctx context.Context) (model.Run, error) {
	run, err := NextQueuedRun(database, ctx)
	if err != nil {
		return model.Run{}, err
	}
	return database.StartRun(ctx, run.ID)
}
