package fake

import (
	"context"
	"encoding/json"
	"net/url"
	"sort"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/reponame"
	"github.com/oberthci/oberth/internal/stateport"
)

func (m *runMemory) appendAudit(v model.AuditAction) {
	v.ID = int64(len(m.audits) + 1)
	v.CreatedAt = m.now
	m.audits = append(m.audits, v)
}

func (f *Runs) RegisterUpstreamBinding(ctx context.Context, a stateport.Action, u stateport.Upstream) error {
	if !validUpstreamBinding(u) {
		return runProblem(publicerr.InvalidInput)
	}
	return f.movingChange(ctx, "RegisterUpstreamBinding", a.Events, func(m *runMemory) error {
		if a.Actor == "" {
			return runProblem(publicerr.InvalidInput)
		}
		if old, ok := m.upstreams[u.Name]; ok {
			if old != u {
				return runProblem(publicerr.Conflict)
			}
			return noRunChange{}
		}
		for _, old := range m.upstreams {
			if old.Org == u.Org || old.Name == u.Org || old.Org == u.Name {
				return runProblem(publicerr.Conflict)
			}
		}
		m.upstreams[u.Name] = u
		m.appendAudit(model.AuditAction{Actor: a.Actor, Action: "upstream.register", ResourceType: "upstream", ResourceID: u.Name, Details: jsonValue(u)})
		return nil
	})
}

func (f *Runs) UpstreamBindings(ctx context.Context) ([]stateport.Upstream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.before(ctx, "UpstreamBindings", false); err != nil {
		return nil, err
	}
	out := []stateport.Upstream{}
	for _, u := range f.data.upstreams {
		if !u.Removed {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *Runs) RecordAdministration(ctx context.Context, a stateport.Action, v model.AuditAction) error {
	if v.Actor != "" && v.Actor != a.Actor {
		return runProblem(publicerr.Forbidden)
	}
	v.Actor = a.Actor
	if a.Reason != "" {
		var details any
		if v.Details != "" {
			details = json.RawMessage(v.Details)
		}
		body, err := json.Marshal(map[string]any{"reason": a.Reason, "details": details})
		if err != nil {
			return runProblem(publicerr.InvalidInput)
		}
		v.Details = string(body)
	}
	return f.RecordAction(ctx, v, a.Events...)
}

func (f *Runs) AdministrativeAudit(ctx context.Context, scope []string, q stateport.AdministrativeFilter) ([]stateport.AuditRecord, error) {
	since, until, err := q.Bounds()
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.before(ctx, "AdministrativeAudit", false); err != nil {
		return nil, err
	}
	all := []stateport.AuditRecord{}
	previous := stateport.AuditGenesis()
	for _, a := range f.data.audits {
		hash := stateport.AuditDigest(stateport.DefaultProtocol().AuditDomain, previous, a)
		all = append(all, stateport.AuditRecord{AuditAction: a, SHA256: hash, PreviousSHA256: previous})
		previous = hash
	}
	out := []stateport.AuditRecord{}
	for i := len(all) - 1; i >= 0; i-- {
		v := all[i]
		if !matchesAdministrativeFilter(q, v.AuditAction) || !since.IsZero() && v.CreatedAt.Before(since) || !until.IsZero() && v.CreatedAt.After(until) || !f.auditVisible(v.AuditAction, scope) {
			continue
		}
		out = append(out, v)
		if len(out) == q.Limit {
			break
		}
	}
	return out, nil
}

func (f *Runs) auditVisible(v model.AuditAction, scope []string) bool {
	if scope == nil {
		return true
	}
	switch v.ResourceType {
	case "repository", "authority", "capability", "schedule":
		return visibleRun(scope, v.ResourceID)
	case "run":
		r, ok := f.data.runs[v.ResourceID]
		return ok && visibleRun(scope, r.Repo)
	}
	return false
}

func validUpstreamBinding(u stateport.Upstream) bool {
	endpoint, err := url.Parse(u.BaseURL)
	return err == nil && reponame.ValidateUpstreamName(u.Name) == nil && endpoint.Host != "" && reponame.ValidateForgeURL(endpoint) == nil && u.Kind != "" && u.KeyName != "" && !u.Removed
}

func matchesAdministrativeFilter(q stateport.AdministrativeFilter, v model.AuditAction) bool {
	return (q.Before == 0 || v.ID < q.Before) && (q.Action == "" || q.Action == v.Action) && (q.Actor == "" || q.Actor == v.Actor)
}
