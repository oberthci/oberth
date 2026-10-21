package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func (service *API) repoArchive(ctx context.Context, actor api.Actor, name string, archived bool) (any, error) {
	if !actor.Admin || actor.Scoped() {
		return nil, fmt.Errorf("%w: repository archive requires an unscoped admin uplink", ErrForbidden)
	}
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("%w: repo is required", ErrInvalidInput)
	}
	if service.repositoryArchiver == nil || service.changeRepositoryArchive == nil {
		return nil, fmt.Errorf("%w: repository archive", ErrUnavailable)
	}
	repository, err := service.runs.RepositoryByName(ctx, name)
	if err != nil {
		return nil, err
	}
	var changed model.Repository
	err = service.changeRepositoryArchive(ctx, service.cacheInputFor(ctx, repository), archived, func() error {
		var err error
		changed, err = service.repositoryArchiver.SetRepositoryArchived(ctx, actor.Identity, name, archived)
		return err
	})
	if errors.Is(err, gitcache.ErrArchivePendingReceive) || errors.Is(err, gitcache.ErrArchiveCacheCleanup) {
		return nil, fmt.Errorf("%w: %w", store.ErrInvalidState, err)
	}
	if err != nil {
		return nil, err
	}
	return changed, nil
}

// refuseArchivedGrant refuses new authority for an archived repository. Its
// existing grants stay readable and revocable, but nothing can run there, so a
// fresh grant would only be dormant authority waiting for an unarchive.
func refuseArchivedGrant(repository model.Repository, tool, name string) error {
	if repository.ArchivedAt == nil {
		return nil
	}
	return fmt.Errorf("%w: %w: %s refused for %s; unarchive the repository first", store.ErrInvalidState, store.ErrRepositoryArchived, tool, name)
}
