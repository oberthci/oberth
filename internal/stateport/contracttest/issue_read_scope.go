package contracttest

import (
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"testing"
)

func checkIssueReadScope(t *testing.T, f AdministrationFixture) {
	a := stateport.Action{Actor: "operator"}
	for _, upstream := range []string{"origin", "other", "global", ""} {
		_, err := f.Port.CreateIssue(t.Context(), a, model.Issue{Kind: model.IssueManual, Title: "task", Upstream: upstream})
		mustRun(t, err)
	}
	for _, allowGlobal := range []bool{false, true} {
		scope := stateport.IssueScope{Upstreams: []string{"origin"}, Global: allowGlobal}
		rows, err := f.Port.ScopedIssues(t.Context(), scope, model.IssueFilter{Limit: 20})
		mustRun(t, err)
		want := 1
		if allowGlobal {
			want = 3
		}
		if len(rows) != want {
			t.Fatal("global visibility is independent of upstream scope", allowGlobal, rows)
		}
		for _, v := range rows {
			if v.Upstream == "other" {
				t.Fatal("foreign issue", v)
			}
			got, err := f.Port.ScopedIssue(t.Context(), scope, v.ID)
			mustRun(t, err)
			if got.ID != v.ID {
				t.Fatal("read/list scope disagreement", got, v)
			}
		}
	}
}
