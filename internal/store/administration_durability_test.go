package store

import (
	"context"
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestAdministrationLateFailureRollsBackWholeDatabase(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"grant", "audit", "upstream"} {
		t.Run(kind, func(t *testing.T) {
			s := testStore(t)
			atomicRepository(t, s)
			e := stateport.EventWrite{Dedupe: "retained", Event: model.AgentEvent{Kind: string(model.EventRunUpdated), Repo: "origin/team/repo", Summary: "retained"}}
			a := stateport.Action{Actor: "operator", Events: []stateport.EventWrite{e}}
			v := model.AuditAction{Actor: a.Actor, Action: "retained", ResourceType: "repository", ResourceID: e.Event.Repo}
			requireOK(t, s.RecordAdministration(t.Context(), a, v))
			conflict := e
			conflict.Event.Summary = "different"
			e.Dedupe = "staged"
			a.Events = []stateport.EventWrite{e, conflict}
			before := databaseContents(t, s.readers)
			if err := changeAdministration(t.Context(), s, a, kind); err == nil {
				t.Fatal("late collision accepted")
			}
			if after := databaseContents(t, s.readers); after != before {
				t.Fatal("late write changed database facts, generation, ownership, audit or events")
			}
		})
	}
}

func changeAdministration(ctx context.Context, s *Store, a stateport.Action, kind string) error {
	switch kind {
	case "grant":
		_, err := s.ChangeGrant(ctx, a, stateport.GrantChange{Kind: stateport.GrantPVC, Repository: "origin/team/repo", Path: "claim", Tier: "ci"})
		return err
	case "audit":
		return s.RecordAdministration(ctx, a, model.AuditAction{Actor: a.Actor, Action: "not committed", ResourceType: "repository", ResourceID: "origin/team/repo"})
	default:
		return s.RegisterUpstreamBinding(ctx, a, stateport.Upstream{Name: "staged", Org: "staged-team", Kind: "git", BaseURL: "https://staged.oberth.ci", KeyName: "source"})
	}
}

func TestAdministrationSurvivesReopen(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	atomicRepository(t, s)
	a := stateport.Action{Actor: "operator", Reason: "approved"}
	g, err := s.ChangeGrant(t.Context(), a, stateport.GrantChange{Kind: stateport.GrantAudience, Repository: "origin/team/repo", File: ".oberth/release.yaml", Step: "publish", Path: "publisher"})
	requireOK(t, err)
	requireOK(t, s.RecordAdministration(t.Context(), a, model.AuditAction{Actor: a.Actor, Action: "backup.completed", ResourceType: "installation", Details: `{"bytes":1}`}))
	records, err := s.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Limit: 200})
	requireOK(t, err)
	bindings, err := s.UpstreamBindings(t.Context())
	requireOK(t, err)
	path, options := s.path, s.options
	requireOK(t, s.Close())
	reopened, err := Open(t.Context(), path, options)
	requireOK(t, err)
	t.Cleanup(func() { requireOK(t, reopened.Close()) })
	got, err := reopened.Grants(t.Context(), nil)
	requireOK(t, err)
	if !reflect.DeepEqual(g, got) {
		t.Fatal("grant generation did not survive", got)
	}
	after, err := reopened.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Limit: 200})
	requireOK(t, err)
	if !reflect.DeepEqual(records, after) {
		t.Fatal("audit chain did not survive", after)
	}
	upstreams, err := reopened.UpstreamBindings(t.Context())
	requireOK(t, err)
	if !reflect.DeepEqual(bindings, upstreams) {
		t.Fatal("upstream bindings did not survive", upstreams)
	}
}
