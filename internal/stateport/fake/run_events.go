package fake

import (
	"context"
	"encoding/json"
	"unicode/utf8"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/reponame"
	"github.com/oberthci/oberth/internal/stateport"
)

func validEvents(events []stateport.EventWrite) error {
	for _, input := range events {
		e := input.Event
		_, repoErr := reponame.ParseRepository(e.Repo)
		if repoErr != nil && !globalEvent(e) || !model.EventKind(e.Kind).Valid() || input.Dedupe == "" || len(input.Dedupe) > 512 || len(e.Data) > model.MaxEventDataBytes || utf8.RuneCountInString(e.Summary) > model.MaxEventSummaryRunes {
			return runProblem(publicerr.InvalidInput)
		}
		if len(e.Data) > 0 && !json.Valid(e.Data) {
			return runProblem(publicerr.InvalidInput)
		}
	}
	return nil
}

func (m *runMemory) appendEvents(events []stateport.EventWrite) error {
	for _, input := range events {
		event := input.Event.Clone()
		event.ID = 0
		if old, ok := m.dedupe[input.Dedupe]; ok {
			if jsonValue(old) != jsonValue(event) {
				return runProblem(publicerr.Conflict)
			}
			continue
		}
		m.dedupe[input.Dedupe] = event.Clone()
		m.eventID++
		event.ID = m.eventID
		m.events = append(m.events, event)
	}
	return nil
}

func (f *Runs) Events(ctx context.Context, q stateport.EventFilter) (model.EventPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := model.EventPage{Events: []model.AgentEvent{}, NextAfterID: q.After}
	if err := f.before(ctx, "Events", false); err != nil {
		return p, err
	}
	if q.After < 0 || q.Through < 0 || q.Limit < 1 || q.Limit > stateport.EventPageLimit {
		return p, runProblem(publicerr.InvalidInput)
	}
	f.data.eventEndpoints(q.ScopeKeys(), &p)
	p.Gap = f.data.eventGap(q)
	upper := q.Through
	if upper == 0 {
		upper = p.LatestID
	}
	for _, e := range f.data.events {
		if !eventRange(e, q, upper) {
			continue
		}
		if !f.data.actorEvent(q.Actor, e) {
			continue
		}
		p.Events = append(p.Events, e.Clone())
		p.NextAfterID = e.ID
		if len(p.Events) == q.Limit {
			break
		}
	}
	if len(p.Events) < q.Limit {
		p.NextAfterID = max(p.NextAfterID, min(p.LatestID, upper))
	}
	p.HasMore = p.NextAfterID < min(p.LatestID, upper)
	return p, nil
}

// RecordAction supplies administrative facts with events to shared fixtures.
func (f *Runs) RecordAction(ctx context.Context, a model.AuditAction, events ...stateport.EventWrite) error {
	if a.Actor == "" || a.Action == "" || a.ResourceType == "" {
		return runProblem(publicerr.InvalidInput)
	}
	return f.movingChange(ctx, "RecordAction", events, func(m *runMemory) error { m.appendAudit(a); return nil })
}

func (m *runMemory) actorEvent(actor string, e model.AgentEvent) bool {
	if actor == "" {
		return true
	}
	r, ok := m.runs[e.RunID]
	return ok && r.Repo == e.Repo && r.Origin.Actor == actor
}
func (m *runMemory) eventEndpoints(scope []string, p *model.EventPage) {
	p.LatestID = m.prunedThrough(scope)
	for _, e := range m.events {
		if visibleRun(scope, stateport.EventScope(e)) {
			if p.OldestID == 0 {
				p.OldestID = e.ID
			}
			p.LatestID = max(p.LatestID, e.ID)
		}
	}
}

func eventRange(e model.AgentEvent, q stateport.EventFilter, upper int64) bool {
	return visibleRun(q.ScopeKeys(), stateport.EventScope(e)) && e.ID > q.After && e.ID <= upper
}

func (m *runMemory) prunedThrough(scope []string) int64 {
	var through int64
	for repo, id := range m.pruned {
		if visibleRun(scope, repo) {
			through = max(through, id)
		}
	}
	return through
}

// PruneEvents supplies retained-history fixtures without adding a service retention port.
func (f *Runs) PruneEvents(ctx context.Context) error {
	return f.movingChange(ctx, "PruneEvents", nil, func(m *runMemory) error {
		if len(m.events) == 0 {
			return noRunChange{}
		}
		for _, e := range m.events {
			m.pruned[stateport.EventScope(e)] = max(m.pruned[stateport.EventScope(e)], e.ID)
		}
		m.events = nil
		clear(m.dedupe)
		return nil
	})
}

func (m *runMemory) eventGap(q stateport.EventFilter) bool {
	return q.After > 0 && q.After < m.prunedThrough(q.ScopeKeys())
}

func globalEvent(v model.AgentEvent) bool { return v.Repo == "" && model.EventKind(v.Kind).Global() }

func (f *Runs) RecordEvents(ctx context.Context, events []stateport.EventWrite) error {
	if len(events) == 0 {
		return nil
	}
	if err := f.before(ctx, "RecordEvents", true); err != nil {
		return err
	}
	return f.transact(ctx, events, func(*runMemory) error { return nil })
}
