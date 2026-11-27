package service

import (
	"context"
	"encoding/json"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/model"
)

// Event history carries bounded summaries, never an opaque diagnostic fallback.
func (s *Service) projectNativeEvents(ctx context.Context, a actor.Actor, page *model.EventPage) {
	for i := range page.Events {
		e := &page.Events[i]
		switch model.EventKind(e.Kind) {
		case model.EventRunUpdated:
			e.Summary = safeNativeReason(e.Summary)
			if e.Summary == "" {
				e.Summary = "run updated"
			}
			e.Data = nil
		case model.EventRunFinished:
			e.Summary = safeNativeReason(e.Summary)
			if e.Summary == "" {
				e.Summary = "run finished"
			}
			e.Data = s.finishedEventData(ctx, a, *e)
		case model.EventRunHeld:
			e.Summary = reasonHeld
			e.Data = s.heldEventData(ctx, a, *e)
		}
	}
}

func (s *Service) finishedEventData(ctx context.Context, a actor.Actor, e model.AgentEvent) json.RawMessage {
	var data struct {
		ResumedFrom string `json:"resumed_from"`
	}
	if json.Unmarshal(e.Data, &data) != nil || data.ResumedFrom == "" {
		return nil
	}
	r, err := s.resolveRun(ctx, a, runQuery{ID: data.ResumedFrom})
	if err != nil || r.Repo != e.Repo {
		return nil
	}
	data.ResumedFrom = r.ID
	body, _ := json.Marshal(data)
	return body
}

func (s *Service) heldEventData(ctx context.Context, a actor.Actor, e model.AgentEvent) json.RawMessage {
	var notice model.HeldNotice
	if json.Unmarshal(e.Data, &notice) != nil || notice.RunID != e.RunID {
		return nil
	}
	r, err := s.resolveRun(ctx, a, runQuery{ID: e.RunID})
	if err != nil || r.Repo != e.Repo || r.Origin.NativeUID() == "" {
		return nil
	}
	nodes, err := s.c.Runs.Nodes(ctx, r.ID)
	if err != nil {
		return nil
	}
	for _, node := range nodes {
		if node.NodeID == notice.NodeID && node.PodUID == notice.PodUID && node.PodUID != "" && node.Path == notice.Step {
			body, _ := json.Marshal(notice)
			return body
		}
	}
	return nil
}
