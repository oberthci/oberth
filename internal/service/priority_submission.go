package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

type priorityGrantStore interface {
	SetPriorityGrant(context.Context, string, model.PriorityGrant) (model.PriorityGrant, error)
	ListPriorityGrants(context.Context) ([]model.PriorityGrant, error)
	RevokePriorityGrant(context.Context, string, string) error
}

func (s *API) priorityGrantTool(ctx context.Context, actor api.Actor, name string, raw json.RawMessage) (any, error) {
	if !actor.Admin || actor.Scoped() {
		return nil, refuse(ErrForbidden, "priority grants require an unscoped admin uplink")
	}
	state, ok := s.queuePriorities.(priorityGrantStore)
	if !ok {
		return nil, refuse(ErrUnavailable, "priority grants")
	}
	if name == "priority_grant_list" {
		if err := decodeTool(raw, &struct{}{}); err != nil {
			return nil, err
		}
		grants, err := state.ListPriorityGrants(ctx)
		return map[string]any{"grants": grants}, err
	}
	if name == "priority_grant_revoke" {
		var a struct {
			Identity string `json:"identity"`
		}
		if err := decodeTool(raw, &a); err != nil {
			return nil, err
		}
		err := state.RevokePriorityGrant(ctx, actor.Identity, a.Identity)
		return map[string]any{"identity": a.Identity, "revoked": err == nil}, err
	}
	var a struct {
		Identity string            `json:"identity"`
		MaxClass model.RunPriority `json:"max_class"`
		Expires  time.Time         `json:"expires"`
	}
	if err := decodeTool(raw, &a); err != nil {
		return nil, err
	}
	return state.SetPriorityGrant(ctx, actor.Identity, model.PriorityGrant{Identity: a.Identity, MaxClass: a.MaxClass, Expires: a.Expires})
}

func requestedPriority(actor api.Actor, request model.PriorityRequest) model.PriorityRequest {
	admin := actor.Admin && !actor.Scoped()
	request.UnscopedAdmin = &admin
	request.RequesterFingerprint = actor.Fingerprint
	return request
}

func firstPriority(requests []model.PriorityRequest) model.PriorityRequest {
	if len(requests) > 0 {
		return requests[0]
	}
	return model.PriorityRequest{}
}

// Share the same admission lock as run_priority so sync cannot change a build
// between its resource grant and durable started_at update.
func (scheduler *Scheduler) RequestRunPriority(ctx context.Context, actor, id string, request model.PriorityRequest) (model.Run, error) {
	state, ok := scheduler.store.(interface {
		RequestRunPriority(context.Context, string, string, model.PriorityRequest, bool) (model.Run, error)
	})
	if !ok {
		return model.Run{}, refuse(ErrUnavailable, "submission priority")
	}
	scheduler.priorityMu.Lock()
	defer scheduler.priorityMu.Unlock()
	g := scheduler.submissions
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, r := range g.owners {
		if r.runID == id && r.granted {
			return state.RequestRunPriority(ctx, actor, id, request, true)
		}
	}
	return state.RequestRunPriority(ctx, actor, id, request, false)
}
