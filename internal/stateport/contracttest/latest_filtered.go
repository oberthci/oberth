package contracttest

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func checkLatestFiltered(t *testing.T, f RunFixture) {
	a := acceptOneRun(t, f, RunCase{ID: "a-alice", Ref: "refs/heads/a", Actor: "alice", Commit: strings.Repeat("a", 40)})
	acceptOneRun(t, f, RunCase{ID: "a-bob", Ref: a.Ref, Actor: "bob"})
	b := acceptOneRun(t, f, RunCase{ID: "b-alice", Ref: "refs/heads/b", Actor: "alice", Commit: strings.Repeat("b", 40)})
	acceptOneRun(t, f, RunCase{ID: "b-bob", Ref: b.Ref, Actor: "bob"})
	acceptOneRun(t, f, RunCase{ID: "tag", Ref: "refs/tags/v1", Actor: "alice", Trigger: model.TriggerTag})
	f.Repository(t, "origin/team/other")
	acceptOneRun(t, f, RunCase{ID: "hidden", Repo: "origin/team/other", Actor: "alice"})
	for _, repository := range []bool{false, true} {
		q := stateport.RunFilter{Repositories: []string{a.Repo}, Actor: "alice", Status: stepmodel.RunQueued, Trigger: model.TriggerBranch,
			RefGlob: "refs/heads/*", Latest: !repository, LatestRepository: repository, Limit: 1}
		rows, err := f.Port.Runs(t.Context(), q)
		mustRun(t, err)
		if len(rows) != 1 || rows[0].ID != b.ID {
			t.Fatal("latest chosen before applying scope/filters", rows)
		}
		q.Cursor = b.ID
		rows, err = f.Port.Runs(t.Context(), q)
		mustRun(t, err)
		if repository {
			if len(rows) != 0 {
				t.Fatal("cursor replaced repository winner with an older run", rows)
			}
		} else if len(rows) != 1 || rows[0].ID != a.ID {
			t.Fatal("filtered latest ref pagination changed", rows)
		}
		q.Cursor, q.CommitSHA, q.Ref = "", *a.CommitSHA, a.Ref
		rows, err = f.Port.Runs(t.Context(), q)
		mustRun(t, err)
		if len(rows) != 1 || rows[0].ID != a.ID {
			t.Fatal("commit/ref filters applied after latest", rows)
		}
	}
}
