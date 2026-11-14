package service

import (
	"context"
	"errors"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func canonicalGrantRepoKey(ctx context.Context, repo string, lookup func(context.Context, string) (model.Repository, error), qualify func(context.Context, int64) (string, error)) (string, error) {
	repository, err := lookup(ctx, repo)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalid) {
		return repo, nil
	}
	if err != nil {
		return "", err
	}
	return qualify(ctx, repository.ID)
}
