package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func (service *API) promoteGrant(ctx context.Context, actor api.Actor, raw json.RawMessage) (any, error) {
	if !actor.Admin {
		return nil, fmt.Errorf("%w: promote_grant requires an admin uplink", ErrForbidden)
	}
	if service.promotionGrants == nil || service.runs == nil {
		return nil, fmt.Errorf("%w: promotion grants", ErrUnavailable)
	}
	var arguments struct {
		Repo    string `json:"repo"`
		SHA     string `json:"sha"`
		Branch  string `json:"branch"`
		Expires string `json:"expires"`
		Note    string `json:"note"`
	}
	if err := decodeTool(raw, &arguments); err != nil {
		return nil, err
	}
	if strings.TrimSpace(arguments.Repo) == "" || strings.TrimSpace(arguments.SHA) == "" || strings.TrimSpace(arguments.Branch) == "" {
		return nil, fmt.Errorf("%w: repo, sha, and branch are required", ErrInvalidInput)
	}
	if !validOID(arguments.SHA) {
		return nil, fmt.Errorf("%w: sha must be a full 40-character commit SHA", ErrInvalidInput)
	}

	repository, err := service.runs.RepositoryByName(ctx, arguments.Repo)
	if err != nil {
		return nil, err
	}

	expiresAt, err := parseExpiry(arguments.Expires, service.promotionGrantMaxLifetime)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}

	grant, err := service.promotionGrants.AppendPromotionGrant(ctx, model.PromotionGrantSpec{
		RepoID:            repository.ID,
		SourceSHA:         arguments.SHA,
		TargetRef:         arguments.Branch,
		Issuer:            actor.Identity,
		IssuerFingerprint: actor.Fingerprint,
		Note:              arguments.Note,
		ExpiresAt:         expiresAt,
	})
	if err != nil {
		return nil, err
	}
	return promotionGrantResponse(grant), nil
}

func (service *API) promoteGrantRevoke(ctx context.Context, actor api.Actor, raw json.RawMessage) (any, error) {
	if !actor.Admin {
		return nil, fmt.Errorf("%w: promote_grant_revoke requires an admin uplink", ErrForbidden)
	}
	if service.promotionGrants == nil {
		return nil, fmt.Errorf("%w: promotion grants", ErrUnavailable)
	}
	var arguments struct {
		ID string `json:"id"`
	}
	if err := decodeTool(raw, &arguments); err != nil {
		return nil, err
	}
	if strings.TrimSpace(arguments.ID) == "" {
		return nil, fmt.Errorf("%w: grant id is required", ErrInvalidInput)
	}
	grant, err := service.promotionGrants.RevokePromotionGrant(ctx, arguments.ID, actor.Identity)
	if err != nil {
		return nil, err
	}
	return promotionGrantResponse(grant), nil
}

func (service *API) promoteGrantList(ctx context.Context, actor api.Actor, raw json.RawMessage) (any, error) {
	if service.promotionGrants == nil || service.runs == nil {
		return nil, fmt.Errorf("%w: promotion grants", ErrUnavailable)
	}
	var arguments struct {
		Repo   string `json:"repo"`
		SHA    string `json:"sha"`
		Status string `json:"status"`
	}
	if err := decodeTool(raw, &arguments); err != nil {
		return nil, err
	}

	filter := model.PromotionGrantListFilter{
		SourceSHA: arguments.SHA,
	}
	if strings.TrimSpace(arguments.Repo) != "" {
		repository, err := service.runs.RepositoryByName(ctx, arguments.Repo)
		if err != nil {
			return nil, err
		}
		filter.RepoID = repository.ID
	}
	switch arguments.Status {
	case "active":
		active := true
		filter.Active = &active
	case "revoked", "expired", "consumed":
		inactive := false
		filter.Active = &inactive
	case "all", "":
		// No filter.
	default:
		return nil, fmt.Errorf("%w: status must be one of: active, revoked, expired, consumed, all", ErrInvalidInput)
	}

	grants, err := service.promotionGrants.ListPromotionGrants(ctx, filter)
	if err != nil {
		return nil, err
	}
	result := make([]map[string]any, 0, len(grants))
	for _, grant := range grants {
		result = append(result, promotionGrantResponse(grant))
	}
	return result, nil
}

func promotionGrantResponse(grant model.PromotionGrant) map[string]any {
	response := map[string]any{
		"id":                 grant.ID,
		"repo_id":            grant.RepoID,
		"source_sha":         grant.SourceSHA,
		"target_ref":         grant.TargetRef,
		"issuer":             grant.Issuer,
		"issuer_fingerprint": grant.IssuerFingerprint,
		"created_at":         grant.CreatedAt.UTC().Format(time.RFC3339Nano),
		"expires_at":         grant.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}
	if grant.Note != "" {
		response["note"] = grant.Note
	}
	if grant.RevokedAt != nil {
		response["revoked_by"] = grant.RevokedBy
		response["revoked_at"] = grant.RevokedAt.UTC().Format(time.RFC3339Nano)
	}
	if grant.ConsumedAt != nil {
		response["consumed_by"] = grant.ConsumedBy
		response["consumed_at"] = grant.ConsumedAt.UTC().Format(time.RFC3339Nano)
	}
	now := time.Now()
	if grant.Active(now) {
		response["status"] = "active"
	} else if grant.RevokedAt != nil {
		response["status"] = "revoked"
	} else if grant.ConsumedAt != nil {
		response["status"] = "consumed"
	} else {
		response["status"] = "expired"
	}
	return response
}

// parseExpiry interprets the expires argument as either an RFC3339 timestamp
// or a Go duration string, clamped to the configured maximum lifetime.
func parseExpiry(value string, maxLifetime time.Duration) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Now().Add(maxLifetime), nil
	}
	// Try RFC3339 first.
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		if t.Before(time.Now()) {
			return time.Time{}, fmt.Errorf("expiry %s is in the past", value)
		}
		if t.After(time.Now().Add(maxLifetime)) {
			return time.Time{}, fmt.Errorf("expiry exceeds maximum lifetime of %s", maxLifetime)
		}
		return t, nil
	}
	// Try duration.
	d, err := time.ParseDuration(value)
	if err != nil {
		return time.Time{}, fmt.Errorf("expires must be an RFC3339 timestamp or a Go duration (e.g. 24h, 1h30m): %w", err)
	}
	if d <= 0 {
		return time.Time{}, fmt.Errorf("expires duration must be positive")
	}
	if d > maxLifetime {
		return time.Time{}, fmt.Errorf("expires duration %s exceeds maximum lifetime of %s", d, maxLifetime)
	}
	return time.Now().Add(d), nil
}
