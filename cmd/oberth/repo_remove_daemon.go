package main

import (
	"context"
	"errors"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/store"
)

// API removal uses the catalog/cache critical section.
type lockedRepositoryRemover struct {
	database *store.Store
	cache    *gitcache.Cache
}

func (r lockedRepositoryRemover) RemoveRepository(ctx context.Context, actor, input string) (store.RemovedRepository, error) {
	var removed store.RemovedRepository
	repo, err := r.database.RepositoryByName(ctx, input)
	if err != nil {
		return removed, err
	}
	upstream, err := r.database.Upstream(ctx, repo.UpstreamID)
	if err != nil {
		return removed, err
	}
	canonical := upstream.Name + "/" + upstream.Org() + "/" + repo.Name
	err = r.cache.RemoveRepositoryWith(ctx, canonical, func() error {
		var err error
		removed, err = r.database.RemoveRepository(ctx, actor, canonical)
		return err
	})
	if err != nil {
		return removed, err
	}
	if _, err := r.database.RepositoryByName(ctx, removed.Name); errors.Is(err, store.ErrNotFound) {
		if err = r.cache.RemoveLegacyRepositoryContext(ctx, removed.Name); err != nil {
			return removed, err
		}
	}
	return removed, nil
}
