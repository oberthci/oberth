package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type UplinkLifecycle interface {
	ListUplinkStatuses(context.Context) ([]store.UplinkStatus, error)
	RevokeUplink(context.Context, string, string, string, string) (model.Uplink, error)
}

type uplinkView struct {
	Identity    string     `json:"identity"`
	Fingerprint string     `json:"fingerprint"`
	Admin       bool       `json:"admin"`
	Scope       []string   `json:"scope"`
	CreatedAt   time.Time  `json:"created_at"`
	LastSeen    *time.Time `json:"last_seen"`
	RevokedAt   *time.Time `json:"revoked_at"`
}

func (service *API) uplinkLifecycleTool(ctx context.Context, actor api.Actor, name string, raw json.RawMessage) (any, error) {
	if name != "uplink_list" {
		if !actor.Admin {
			return nil, fmt.Errorf("%w: %s requires admin", ErrForbidden, name)
		}
		if err := requireUnscoped(actor, name); err != nil {
			return nil, err
		}
	}
	if service.uplinkLifecycle == nil {
		return nil, fmt.Errorf("%w: uplink lifecycle", ErrUnavailable)
	}
	if name == "uplink_list" {
		var args struct{}
		if err := decodeTool(raw, &args); err != nil {
			return nil, err
		}
		statuses, err := service.uplinkLifecycle.ListUplinkStatuses(ctx)
		if err != nil {
			return nil, err
		}
		views := []uplinkView{}
		for _, status := range statuses {
			if (!actor.Admin || actor.Scoped()) && (status.Identity != actor.Identity || status.Fingerprint != actor.Fingerprint) {
				continue
			}
			scope := append([]string{}, status.Scope...)
			var seen *time.Time
			if !status.LastUsedAt.IsZero() {
				at := status.LastUsedAt
				seen = &at
			}
			views = append(views, uplinkView{Identity: status.Identity, Fingerprint: status.Fingerprint, Admin: status.Admin, Scope: scope, CreatedAt: status.CreatedAt, LastSeen: seen, RevokedAt: status.RevokedAt})
		}
		return map[string]any{"uplinks": views}, nil
	}
	var args struct {
		Identity    string `json:"identity"`
		Fingerprint string `json:"fingerprint"`
		Replacement string `json:"replacement_fingerprint"`
	}
	if err := decodeTool(raw, &args); err != nil {
		return nil, err
	}
	if (name == "uplink_rotate") != (args.Replacement != "") {
		return nil, fmt.Errorf("%w: replacement_fingerprint is required only for uplink_rotate", ErrInvalidInput)
	}
	value, err := service.uplinkLifecycle.RevokeUplink(ctx, actor.Identity, args.Identity, args.Fingerprint, args.Replacement)
	if errors.Is(err, store.ErrInvalid) {
		return nil, fmt.Errorf("%w: %s", ErrInvalidInput, err.Error())
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"identity": value.Identity, "fingerprint": value.Fingerprint, "revoked": true}, nil
}

func (service *API) dismissAccess(ctx context.Context, actor api.Actor, raw json.RawMessage) (any, error) {
	if !actor.Admin {
		return nil, fmt.Errorf("%w: access_dismiss requires admin", ErrForbidden)
	}
	var args struct {
		Repo   string `json:"repo"`
		Step   string `json:"step"`
		Secret string `json:"secret"`
	}
	if err := decodeTool(raw, &args); err != nil {
		return nil, err
	}
	if service.declarationDismissal == nil {
		return nil, fmt.Errorf("%w: access dismissal", ErrUnavailable)
	}
	repo := args.Repo
	if actor.Scoped() {
		repository, err := service.runs.RepositoryByName(ctx, args.Repo)
		if err != nil {
			return nil, err
		}
		repo = service.cacheInputFor(ctx, repository)
	}
	if err := service.declarationDismissal.DismissGrantDeclaration(ctx, actor.Identity, repo, args.Step, args.Secret); err != nil {
		if errors.Is(err, store.ErrInvalid) {
			return nil, fmt.Errorf("%w: %s", ErrInvalidInput, err.Error())
		}
		return nil, err
	}
	return map[string]any{"dismissed": true, "repo": repo, "step": args.Step, "secret": args.Secret}, nil
}

type DeclarationDismissal interface {
	DismissGrantDeclaration(context.Context, string, string, string, string) error
}

func (service *API) listKubeVirtGrants(ctx context.Context, actor api.Actor, raw json.RawMessage) (any, error) {
	if !actor.Admin {
		return nil, fmt.Errorf("%w: kubevirt_grant_list requires admin", ErrForbidden)
	}
	var args struct{}
	if err := decodeTool(raw, &args); err != nil {
		return nil, err
	}
	if service.kubeVirtGrants == nil || service.repositories == nil {
		return nil, fmt.Errorf("%w: KubeVirt grants", ErrUnavailable)
	}
	repos, err := service.repositories.ListRepositories(ctx)
	if err != nil {
		return nil, err
	}
	grants := []map[string]any{}
	for _, repo := range repos {
		grant, err := service.kubeVirtGrants.KubeVirtGrant(ctx, repo.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if grant.RevokedAt != nil {
			continue
		}
		grants = append(grants, map[string]any{"repo": service.cacheInputFor(ctx, repo), "grant": grant})
	}
	return map[string]any{"grants": grants}, nil
}
