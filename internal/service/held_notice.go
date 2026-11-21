package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

// Held appends an immutable observation. It changes neither controls nor outcomes.
func (s *Service) Held(ctx context.Context, h model.HeldNotice) error {
	if h.HeldSince.IsZero() || h.TTLSeconds <= 0 || h.TTLSeconds > 14400 || h.NodeID == "" || h.PodUID == "" {
		return bad("hold")
	}
	r, err := s.c.Runs.Run(ctx, h.RunID)
	if err != nil {
		return err
	}
	if !r.Origin.AssistRequested {
		return refuse(holdInactive)
	}
	h.HeldSince = h.HeldSince.UTC()
	payload, err := json.Marshal(h)
	if err != nil {
		return err
	}
	identity, _ := json.Marshal([]string{h.RunID, h.Step, h.NodeID, h.PodUID})
	event := model.AgentEvent{Kind: string(model.EventRunHeld), At: h.HeldSince, Repo: r.Repo, RunID: r.ID, Ref: r.Ref, ObjectSHA: r.ObjectSHA, CommitSHA: r.CommitSHA, Summary: reasonHeld, Data: payload}
	return s.c.Events.RecordEvents(ctx, []stateport.EventWrite{{Event: event, Dedupe: fmt.Sprintf("run.held:%x", sha256.Sum256(identity))}})
}
