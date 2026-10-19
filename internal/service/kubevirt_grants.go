package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/store"
)

func (service *API) kubeVirtGrantTool(ctx context.Context, actor api.Actor, name, repoName string) (any, error) {
	if service.kubeVirtGrants == nil || service.repositories == nil {
		return nil, fmt.Errorf("%w: KubeVirt grants", ErrUnavailable)
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
		return map[string]any{"repo": repoName, "granted": grant.RevokedAt == nil, "state": state, "grant": grant}, nil
	}
	if !actor.Admin {
		return nil, fmt.Errorf("%w: KubeVirt grants require an admin uplink", ErrForbidden)
	}
	grant, err := service.kubeVirtGrants.SetKubeVirtGrant(ctx, repository.ID, actor.Identity, name == "kubevirt_grant")
	if err != nil {
		return nil, err
	}
	return map[string]any{"repo": repoName, "granted": grant.RevokedAt == nil, "grant": grant}, nil
}
