package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// sizeGrantTool serves size_grant, size_revoke and size_grant_get (#1021),
// mirroring the KubeVirt grant: scoped lookup, admin-only audited mutation.
func (service *API) sizeGrantTool(ctx context.Context, actor api.Actor, name, repoName, maximum string) (any, error) {
	if service.sizeGrants == nil || service.repositories == nil {
		return nil, refuse(ErrUnavailable, "size grants")
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
	if name == "size_grant_get" {
		current, err := service.sizeGrants.MaxRunSize(ctx, repository.ID)
		if err != nil {
			return nil, err
		}
		grant, err := service.sizeGrants.SizeGrant(ctx, repository.ID)
		if errors.Is(err, store.ErrNotFound) {
			return map[string]any{"repo": repoName, "max_size": current, "state": "never_granted"}, nil
		}
		if err != nil {
			return nil, err
		}
		state := "granted"
		if grant.RevokedAt != nil {
			state = "revoked"
		}
		return map[string]any{"repo": repoName, "max_size": current, "state": state, "grant": grant}, nil
	}
	if !actor.Admin {
		return nil, refuse(ErrForbidden, "size grants require an admin uplink")
	}
	var size periapsis.Size
	if name == "size_grant" {
		if err := refuseArchivedGrant(repository, name, repoName); err != nil {
			return nil, err
		}
		size = periapsis.Size(strings.TrimSpace(maximum))
		if !store.ValidSizeGrantMax(size) {
			return nil, fmt.Errorf("%w: max must be %s or %s", ErrInvalidInput, periapsis.L, periapsis.XL)
		}
	}
	grant, err := service.sizeGrants.SetSizeGrant(ctx, repository.ID, actor.Identity, size)
	if err != nil {
		return nil, err
	}
	current := store.DefaultMaxRunSize
	if grant.RevokedAt == nil {
		current = grant.MaxSize
	}
	return map[string]any{"repo": repoName, "max_size": current, "grant": grant}, nil
}
