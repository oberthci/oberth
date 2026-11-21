package contracttest

import (
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func checkIssueWriteScope(t *testing.T, f AdministrationFixture) {
	mustRun(t, f.Port.RegisterUpstreamBinding(t.Context(), stateport.Action{Actor: "operator"}, stateport.Upstream{Name: "origin", Org: "team", Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source"}))
	a := stateport.Action{Actor: "worker", Caller: actor.Actor{Identity: "worker", AuthMethod: "key", Scope: []string{"origin"}}}
	before := administrationState(t, f)
	for _, upstream := range []string{"other", "global", ""} {
		if _, err := f.Port.CreateIssue(t.Context(), a, model.Issue{Kind: model.IssueManual, Title: "forbidden", Upstream: upstream}); err == nil {
			t.Fatal("scoped issue create escaped upstream", upstream)
		}
	}
	if !reflect.DeepEqual(before, administrationState(t, f)) {
		t.Fatal("refused issue create wrote audit or events")
	}
	v, err := f.Port.CreateIssue(t.Context(), a, model.Issue{Kind: model.IssueManual, Title: "visible", Upstream: "origin"})
	mustRun(t, err)
	a.Caller.Scope = []string{"origin", "other"}
	if _, err = f.Port.UpdateIssue(t.Context(), a, model.IssuePatch{ID: v.ID, Upstream: new("other")}); err == nil {
		t.Fatal("non-admin moved upstream")
	}
	a.Caller.Admin = true
	if _, err = f.Port.UpdateIssue(t.Context(), a, model.IssuePatch{ID: v.ID, Upstream: new("other")}); err == nil {
		t.Fatal("scoped admin moved upstream")
	}
	a.Caller.Scope = nil
	_, err = f.Port.UpdateIssue(t.Context(), a, model.IssuePatch{ID: v.ID, Upstream: new("other")})
	mustRun(t, err)
}
