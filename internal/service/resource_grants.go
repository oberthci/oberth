package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/store"
)

type ResourceGrantStore interface {
	ResourceGrant(context.Context, int64) (store.ResourceGrant, error)
	ResourceCeiling(context.Context, int64) (corev1.ResourceRequirements, error)
	SetResourceCeiling(context.Context, int64, string, *corev1.ResourceRequirements) (store.ResourceGrant, error)
}

func (s *API) resourceGrantTool(ctx context.Context, actor api.Actor, name, repo string, resources corev1.ResourceRequirements) (any, error) {
	grants, ok := s.sizeGrants.(ResourceGrantStore)
	if !ok || s.repositories == nil {
		return nil, refuse(ErrUnavailable, "resource grants")
	}
	if strings.TrimSpace(repo) == "" {
		return nil, fmt.Errorf("%w: repository required", ErrInvalidInput)
	}
	repository, err := s.repositories.RepositoryByName(ctx, repo)
	if err != nil {
		return nil, err
	}
	if s.scope != nil && !s.scope.seesRepo(repository.ID) {
		return nil, hiddenRepository(repo)
	}
	if name != "resource_grant_get" {
		if !actor.Admin {
			return nil, refuse(ErrForbidden, "resource grants require an admin uplink")
		}
		var ceiling *corev1.ResourceRequirements
		if name == "resource_grant" {
			if err = refuseArchivedGrant(repository, name, repo); err != nil {
				return nil, err
			}
			ceiling = &resources
		}
		if _, err = grants.SetResourceCeiling(ctx, repository.ID, actor.Identity, ceiling); err != nil {
			return nil, err
		}
	}
	ceiling, err := grants.ResourceCeiling(ctx, repository.ID)
	if err != nil {
		return nil, err
	}
	grant, err := grants.ResourceGrant(ctx, repository.ID)
	if errors.Is(err, store.ErrNotFound) {
		return map[string]any{"repo": repo, "resource_ceiling": ceiling, "state": "never_granted"}, nil
	}
	if err != nil {
		return nil, err
	}
	state := "granted"
	if grant.RevokedAt != nil {
		state = "revoked"
	}
	return map[string]any{"repo": repo, "resource_ceiling": ceiling, "state": state, "grant": grant}, nil
}
