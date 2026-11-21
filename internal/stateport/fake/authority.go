package fake

import (
	"context"
	"fmt"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"sort"
)

func grantKey(g stateport.GrantChange) string {
	return fmt.Sprintf("%s:%s:%s:%s:%s:%s", g.Kind, g.Repository, g.File, g.Step, g.Path, g.Tier)
}
func (m *runMemory) grantSnapshot(scope []string) stateport.AuthoritySnapshot {
	v := stateport.AuthoritySnapshot{Revision: m.grantRevision, Grants: []stateport.GrantChange{}}
	for _, g := range m.grants {
		if visibleRun(scope, g.Repository) {
			v.Grants = append(v.Grants, g.Clone())
		}
	}
	sort.Slice(v.Grants, func(i, j int) bool { return grantKey(v.Grants[i]) < grantKey(v.Grants[j]) })
	return v
}
func (f *Runs) ChangeGrant(ctx context.Context, a stateport.Action, g stateport.GrantChange) (stateport.AuthoritySnapshot, error) {
	var out stateport.AuthoritySnapshot
	err := f.movingChange(ctx, "ChangeGrant", a.Events, func(m *runMemory) error {

		if err := m.grantBinding(a, g); err != nil {
			return err
		}
		old, exists := m.grants[grantKey(g)]
		if g.Dismiss && exists && (!old.Dismiss || old.Revoke) {
			reason, remedy := stateport.GrantDismissalRefusal()
			return publicerr.New(publicerr.InvalidInput, reason, remedy, nil)
		}

		m.grants[grantKey(g)] = g.Clone()
		m.grantRevision++
		m.appendAudit(model.AuditAction{Actor: a.Actor, Action: "authority.changed", ResourceType: "authority", ResourceID: g.Repository, Details: jsonValue(map[string]any{"reason": a.Reason, "grant": g})})
		out = m.grantSnapshot(nil)
		return nil
	})
	if err != nil {
		return stateport.AuthoritySnapshot{}, err
	}
	return out, nil
}
func (f *Runs) Grants(ctx context.Context, scope []string) (stateport.AuthoritySnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.before(ctx, "Grants", false); err != nil {
		return stateport.AuthoritySnapshot{}, err
	}
	return f.data.grantSnapshot(scope), nil
}

func (m *runMemory) grantBinding(a stateport.Action, g stateport.GrantChange) error {
	if a.Actor == "" || !g.Kind.Valid() {
		return runProblem(publicerr.InvalidInput)
	}
	if _, ok := m.repos[g.Repository]; !ok {
		return runProblem(publicerr.NotFound)
	}
	if g.Kind != stateport.GrantPVC {
		return nil
	}
	for _, old := range m.grants {
		if old.Kind == stateport.GrantPVC && old.Path == g.Path && (old.Repository != g.Repository || old.Tier != g.Tier) {
			return runProblem(publicerr.Conflict)
		}
	}
	return nil
}
