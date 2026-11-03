package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/store"
)

func (service *API) kubeVirtGrantTool(ctx context.Context, actor api.Actor, name, repoName string, budget store.KubeVirtBudget) (any, error) {
	if service.kubeVirtGrants == nil || service.repositories == nil {
		return nil, refuse(ErrUnavailable, "KubeVirt grants")
	}
	if strings.TrimSpace(repoName) == "" {
		return nil, fmt.Errorf("%w: repository is required", ErrInvalidInput)
	}
	repository, err := service.repositories.RepositoryByName(ctx, repoName)
	if err != nil {
		return nil, err
	}
	if service.scope != nil && !service.scope.seesRepo(repository.ID) {
		return nil, hiddenRepository(repoName)
	}
	if name == "kubevirt_grant_get" {
		grant, err := service.kubeVirtGrants.KubeVirtGrant(ctx, repository.ID)
		if errors.Is(err, store.ErrNotFound) {
			return map[string]any{"repo": repoName, "granted": false, "state": "never_granted"}, nil
		}
		if err != nil {
			return nil, err
		}
		state := "granted"
		if grant.RevokedAt != nil {
			state = "revoked"
		}
		return map[string]any{"repo": repoName, "granted": grant.RevokedAt == nil, "state": state, "grant": grant, "max_memory": grant.MaxMemory()}, nil
	}
	if !actor.Admin {
		return nil, refuse(ErrForbidden, "KubeVirt grants require an admin uplink")
	}
	if name == "kubevirt_grant" {
		if err := refuseArchivedGrant(repository, name, repoName); err != nil {
			return nil, err
		}
	}
	grant, err := service.kubeVirtGrants.SetKubeVirtGrant(ctx, repository.ID, actor.Identity, name == "kubevirt_grant", budget)
	if errors.Is(err, store.ErrInvalid) {
		return nil, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"repo": repoName, "granted": grant.RevokedAt == nil, "grant": grant, "max_memory": grant.MaxMemory()}, nil
}
