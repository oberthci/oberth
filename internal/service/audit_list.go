package service

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

// AuditLister is an optional capability an Auditor may implement to list
// audit actions for the audit_list tool (#1104). Every returned action has
// been recomputed against its stored predecessor link.
type AuditLister interface {
	ListAuditActions(ctx context.Context, filter model.AuditListFilter) ([]model.AuditAction, int64, error)
}

// AuditEntryView is one audit_list entry: the action and its position in the
// hash chain. Verified is always true; an entry whose hash does not recompute
// fails the call instead of being returned.
type AuditEntryView struct {
	ID             int64           `json:"id"`
	Time           string          `json:"time"`
	Actor          string          `json:"actor"`
	Action         string          `json:"action"`
	ResourceType   string          `json:"resource_type"`
	ResourceID     string          `json:"resource_id"`
	Details        json.RawMessage `json:"details"`
	SHA256         string          `json:"sha256"`
	PreviousSHA256 string          `json:"previous_sha256"`
	Verified       bool            `json:"verified"`
}

// AuditListResponse is the audit_list result. NextBefore is set on a full
// page: pass it as before for older entries until it is absent.
type AuditListResponse struct {
	Entries    []AuditEntryView `json:"entries"`
	HeadID     int64            `json:"head_id"`
	NextBefore int64            `json:"next_before,omitempty"`
}

func (response AuditListResponse) AuditNextBefore() int64 { return response.NextBefore }

const auditListMaxLimit = 200

// auditList serves audit_list. It is admin-only: audit rows span uplinks,
// upstreams and the secret store, and a scoped uplink gets no rows at all
// rather than a filtered view.
func (service *API) auditList(ctx context.Context, actor api.Actor, raw json.RawMessage) (any, error) {
	if err := requireUnscoped(actor, "audit_list"); err != nil {
		return nil, err
	}
	if !actor.Admin {
		return nil, fmt.Errorf("%w: audit_list requires an admin uplink", ErrForbidden)
	}
	var arguments struct {
		Action string `json:"action"`
		Actor  string `json:"actor"`
		Repo   string `json:"repo"`
		Since  string `json:"since"`
		Until  string `json:"until"`
		Before int64  `json:"before"`
		Limit  int    `json:"limit"`
	}
	if err := decodeTool(raw, &arguments); err != nil {
		return nil, err
	}
	lister, ok := service.auditor.(AuditLister)
	if !ok || lister == nil {
		return nil, fmt.Errorf("%w: audit listing", ErrUnavailable)
	}
	if arguments.Before < 0 || arguments.Limit < 0 {
		return nil, fmt.Errorf("%w: before and limit must not be negative", ErrInvalidInput)
	}
	now := time.Now()
	filter := model.AuditListFilter{ActionPrefix: arguments.Action, Actor: arguments.Actor, Before: arguments.Before, Limit: arguments.Limit}
	if filter.Limit == 0 {
		filter.Limit = 50
	}
	if filter.Limit > auditListMaxLimit {
		filter.Limit = auditListMaxLimit
	}
	var err error
	if filter.Since, err = parseAuditTime("since", arguments.Since, now); err != nil {
		return nil, err
	}
	if filter.Until, err = parseAuditTime("until", arguments.Until, now); err != nil {
		return nil, err
	}
	if strings.TrimSpace(arguments.Repo) != "" {
		if service.runs == nil {
			return nil, fmt.Errorf("%w: repositories", ErrUnavailable)
		}
		repository, err := service.runs.RepositoryByName(ctx, arguments.Repo)
		if err != nil {
			return nil, err
		}
		filter.RepoID, filter.RepoName = repository.ID, repository.Name
	}
	actions, head, err := lister.ListAuditActions(ctx, filter)
	if err != nil {
		return nil, err
	}
	response := AuditListResponse{Entries: make([]AuditEntryView, 0, len(actions)), HeadID: head}
	for _, action := range actions {
		details := json.RawMessage(action.Details)
		if !json.Valid(details) {
			details = json.RawMessage(`{}`)
		}
		response.Entries = append(response.Entries, AuditEntryView{
			ID: action.ID, Time: action.CreatedAt.UTC().Format(time.RFC3339Nano), Actor: action.Actor, Action: action.Action,
			ResourceType: action.ResourceType, ResourceID: action.ResourceID, Details: details,
			SHA256: hex.EncodeToString(action.SHA256), PreviousSHA256: hex.EncodeToString(action.PreviousSHA256), Verified: true,
		})
	}
	if len(actions) == filter.Limit {
		response.NextBefore = actions[len(actions)-1].ID
	}
	return response, nil
}

// parseAuditTime accepts an RFC 3339 instant or a Go duration ("24h") meaning
// that long before now.
func parseAuditTime(name, value string, now time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	if instant, err := time.Parse(time.RFC3339, value); err == nil {
		return instant, nil
	}
	if duration, err := time.ParseDuration(value); err == nil && duration > 0 {
		return now.Add(-duration), nil
	}
	return time.Time{}, fmt.Errorf("%w: %s must be an RFC 3339 time or a positive duration such as 24h", ErrInvalidInput, name)
}
