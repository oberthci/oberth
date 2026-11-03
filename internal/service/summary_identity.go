package service

import (
	"context"
	"fmt"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// repositoryIdentity reads only the caller-visible repository and upstream.
func (service *API) repositoryIdentity(ctx context.Context, id int64) (string, error) {
	repo, err := service.identityRepository(ctx, id)
	if err != nil {
		return "", fmt.Errorf("read repository identity: %w", err)
	}
	if resolver, ok := service.runs.(upstreamResolver); ok {
		upstream, err := resolver.Upstream(ctx, repo.UpstreamID)
		if err != nil {
			return "", fmt.Errorf("read repository upstream: %w", err)
		}
		return upstream.QualifiedRepo(repo.Name), nil
	}
	return repo.Name, nil
}

func (service *API) identityRepository(ctx context.Context, id int64) (model.Repository, error) {
	if service.repositories != nil {
		return service.repositories.Repository(ctx, id)
	}
	repos, err := service.runs.ListRepositories(ctx)
	if err != nil {
		return model.Repository{}, err
	}
	for _, repo := range repos {
		if repo.ID == id {
			return repo, nil
		}
	}
	return model.Repository{}, fmt.Errorf("%w: repository", store.ErrNotFound)
}
