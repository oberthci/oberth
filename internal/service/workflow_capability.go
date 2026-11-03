package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func (s *API) workflowCapabilityTool(ctx context.Context, actor api.Actor, name, repoName string, capability *argoworkflow.Capability) (any, error) {
	if s.workflowCapabilities == nil || s.repositories == nil {
		return nil, refuse(ErrUnavailable, "workflow capabilities")
	}
	if strings.TrimSpace(repoName) == "" {
		return nil, fmt.Errorf("%w: repository is required", ErrInvalidInput)
	}
	repo, err := s.repositories.RepositoryByName(ctx, repoName)
	if err != nil {
		return nil, err
	}
	if s.scope != nil && !s.scope.seesRepo(repo.ID) {
		return nil, hiddenRepository(repoName)
	}
	var grant store.WorkflowCapabilityGrant
	if name == "workflow_capability_get" {
		grant, err = s.workflowCapabilities.WorkflowCapabilityGrant(ctx, repo.ID)
		if errors.Is(err, store.ErrNotFound) {
			return map[string]any{"repo": repoName, "granted": false, "state": "never_granted"}, nil
		}
	} else {
		if !actor.Admin {
			return nil, refuse(ErrForbidden, "workflow capabilities require an admin uplink")
		}
		if name == "workflow_capability_grant" {
			if err = refuseArchivedGrant(repo, name, repoName); err != nil {
				return nil, err
			}
		}
		grant, err = s.workflowCapabilities.SetWorkflowCapabilityGrant(ctx, repo.ID, actor.Identity, capability)
	}
	if errors.Is(err, store.ErrInvalid) {
		return nil, fmt.Errorf("%w: %w", ErrInvalidInput, err)
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
