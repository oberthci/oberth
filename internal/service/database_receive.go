package service

import (
	"context"

	"github.com/oberthci/oberth/internal/model"
)

// Replay may perform slow Git work. Retry only its storage boundaries, leaving
// ordinary live receive requests (without a background scope) unchanged.
type databaseRepositories struct{ RepositoryWriter }

func (d databaseRepositories) RepositoryByName(ctx context.Context, name string) (model.Repository, error) {
	return retryDatabase(ctx, "RepositoryByName", func(ctx context.Context) (model.Repository, error) {
		return d.RepositoryWriter.RepositoryByName(ctx, name)
	})
}
func (d databaseRepositories) CreateRepository(ctx context.Context, spec model.RepositorySpec) (model.Repository, error) {
	return retryDatabase(ctx, "CreateRepository", func(ctx context.Context) (model.Repository, error) {
		return d.RepositoryWriter.CreateRepository(ctx, spec)
	})
}
func (d databaseRepositories) SetRepositoryDefaultBranch(ctx context.Context, id int64, branch string) (model.Repository, error) {
	return retryDatabase(ctx, "SetRepositoryDefaultBranch", func(ctx context.Context) (model.Repository, error) {
		return d.RepositoryWriter.SetRepositoryDefaultBranch(ctx, id, branch)
	})
}

type databaseReceives struct{ ReceiveRecorder }

func (d databaseReceives) RecordReceiveEvent(ctx context.Context, spec model.ReceiveEventSpec) (bool, error) {
	return retryDatabase(ctx, "RecordReceiveEvent", func(ctx context.Context) (bool, error) { return d.ReceiveRecorder.RecordReceiveEvent(ctx, spec) })
}
