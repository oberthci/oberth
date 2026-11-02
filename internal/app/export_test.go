package app

import (
	"context"
	"sync"
	"time"
)

type MemoryScheduleState struct {
	mu    sync.Mutex
	fires map[string]map[string]time.Time
}

func NewMemoryScheduleState() *MemoryScheduleState {
	return &MemoryScheduleState{fires: map[string]map[string]time.Time{}}
}

func (state *MemoryScheduleState) ScheduleFires(_ context.Context, repo string) (map[string]time.Time, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	copied := map[string]time.Time{}
	for name, when := range state.fires[repo] {
		copied[name] = when
	}
	return copied, nil
}

func (state *MemoryScheduleState) RecordScheduleFire(_ context.Context, repo, entry string, at time.Time, outcome string) error {
	if entry == "" || (outcome != "fired" && outcome != "coalesced" && outcome != "skipped" && outcome != "failed" && outcome != "paused") {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.fires[repo] == nil {
		state.fires[repo] = map[string]time.Time{}
	}
	if outcome == "fired" || outcome == "coalesced" {
		state.fires[repo][entry] = at.UTC()
	} else if _, seen := state.fires[repo][entry]; !seen {
		state.fires[repo][entry] = time.Time{}
	}
	return nil
}
