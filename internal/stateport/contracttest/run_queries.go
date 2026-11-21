package contracttest

import (
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func checkRunQueries(t *testing.T, f RunFixture) {
	old := acceptOneRun(t, f, RunCase{ID: "branch", Actor: "alice", Ref: "refs/heads/topic/nested"})
	acceptOneRun(t, f, RunCase{ID: "tag", Actor: "bob", Ref: "refs/tags/v1", Trigger: model.TriggerTag})
	acceptOneRun(t, f, RunCase{ID: "other", Actor: "bob"})
	for _, q := range []stateport.RunFilter{
		{Actor: "alice", Limit: 1},
		{Actor: "alice", RefGlob: "refs/heads/*", CommitSHA: *old.CommitSHA, Trigger: model.TriggerBranch, Limit: 1},
	} {
		rows, err := f.Port.Runs(t.Context(), q)
		mustRun(t, err)
		if len(rows) != 1 || rows[0].ID != old.ID {
			t.Fatal("filters applied after limit", q, rows)
		}
	}
	q := stateport.RunFilter{Trigger: model.TriggerBranch, CommitSHA: *old.CommitSHA, Limit: 200}
	rows, err := f.Port.Runs(t.Context(), q)
	mustRun(t, err)
	for _, r := range rows {
		if r.Origin.Trigger != model.TriggerBranch {
			t.Fatal("tag crossed branch wait")
		}
	}
	for _, q := range []stateport.RunFilter{{Limit: 201}, {Limit: 0}, {Before: -1, Limit: 1}, {RefGlob: "[", Limit: 1}, {RefGlob: "refs/heads/\\*", Limit: 1}, {Cursor: "bad\x00id", Limit: 1}, {Trigger: "unknown", Limit: 1}} {
		_, err = f.Port.Runs(t.Context(), q)
		runCode(t, err, publicerr.InvalidInput)
		if q.Trigger != "" && !strings.Contains(err.Error(), "trigger") {
			t.Fatal("trigger refusal lost field", err)
		}
	}
	rows, err = f.Port.Runs(t.Context(), stateport.RunFilter{Repositories: []string{}, Limit: 1})
	mustRun(t, err)
	if len(rows) != 0 {
		t.Fatal("empty scope widened")
	}
}

func checkLatestCursor(t *testing.T, f RunFixture) {
	acceptOneRun(t, f, RunCase{ID: "a-old", Ref: "refs/heads/a"})
	b := acceptOneRun(t, f, RunCase{ID: "b", Ref: "refs/heads/b"})
	a := acceptOneRun(t, f, RunCase{ID: "a-new", Ref: "refs/heads/a"})
	q := stateport.RunFilter{Latest: true, Limit: 1}
	rows, err := f.Port.Runs(t.Context(), q)
	mustRun(t, err)
	if len(rows) != 1 || rows[0].ID != a.ID {
		t.Fatal("wrong newest ref")
	}
	q.Cursor = a.ID
	rows, err = f.Port.Runs(t.Context(), q)
	mustRun(t, err)
	if len(rows) != 1 || rows[0].ID != b.ID {
		t.Fatal("latest grouping or cursor unstable", rows)
	}
	q.Cursor = b.ID
	rows, err = f.Port.Runs(t.Context(), q)
	mustRun(t, err)
	if len(rows) != 0 {
		t.Fatal("older run leaked through latest cursor", rows)
	}
	q.Cursor = "missing"
	_, err = f.Port.Runs(t.Context(), q)
	runCode(t, err, publicerr.NotFound)
	q.Cursor = a.ID
	q.Repositories = []string{}
	_, err = f.Port.Runs(t.Context(), q)
	runCode(t, err, publicerr.NotFound)
}

func checkLatestRepository(t *testing.T, f RunFixture) {
	f.Repository(t, "origin/team/other")
	acceptOneRun(t, f, RunCase{ID: "other", Repo: "origin/team/other"})
	for i := range 201 {
		acceptOneRun(t, f, RunCase{ID: fmt.Sprint("ref-", i), Ref: fmt.Sprintf("refs/heads/ref-%d", i)})
	}
	rows, err := f.Port.Runs(t.Context(), stateport.RunFilter{LatestRepository: true, Limit: 2})
	mustRun(t, err)
	if len(rows) != 2 || rows[0].Repo == rows[1].Repo {
		t.Fatal("one repository crowded out another", rows)
	}
}

func checkCommitPrefix(t *testing.T, f RunFixture) {
	f.Repository(t, "hidden/other/repo")
	commit := strings.Repeat("a", 39) + "1"
	acceptOneRun(t, f, RunCase{ID: "old", Commit: commit})
	newest := acceptOneRun(t, f, RunCase{ID: "new", Commit: commit})
	acceptOneRun(t, f, RunCase{ID: "hidden", Repo: "hidden/other/repo", Commit: strings.Repeat("a", 39) + "2"})
	found, err := f.Port.RunByCommit(t.Context(), []string{newest.Repo}, "aaaa")
	mustRun(t, err)
	if found.ID != newest.ID {
		t.Fatal("prefix did not choose newest visible commit")
	}
	object := acceptOneRun(t, f, RunCase{ID: "object", Commit: strings.Repeat("c", 40), Object: strings.Repeat("b", 40), Trigger: model.TriggerTag})
	for _, query := range []struct{ prefix, id string }{{"aAaA", newest.ID}, {"bbbb", object.ID}} {
		found, err := f.Port.RunByCommit(t.Context(), []string{newest.Repo}, query.prefix)
		mustRun(t, err)
		if found.ID != query.id {
			t.Fatal("prefix lost case folding or pushed object", query, found.ID)
		}
	}
	for _, prefix := range []string{"", "not-hex", strings.Repeat("a", 65)} {
		_, err = f.Port.RunByCommit(t.Context(), nil, prefix)
		runCode(t, err, publicerr.InvalidInput)
	}
	_, err = f.Port.RunByCommit(t.Context(), nil, "aaaa")
	runCode(t, err, publicerr.Ambiguous)
	_, err = f.Port.ResolveRun(t.Context(), []string{}, newest.ID)
	runCode(t, err, publicerr.NotFound)
}

func checkScopeCatalog(t *testing.T, f RunFixture) {
	all, err := f.Port.RepositoryScope(t.Context(), nil)
	mustRun(t, err)
	if all != nil {
		t.Fatal("nil scope lost all meaning")
	}
	for _, upstreams := range [][]string{{}, {"unknown"}, {"origin/team/repo"}} {
		names, err := f.Port.RepositoryScope(t.Context(), upstreams)
		mustRun(t, err)
		if names == nil || len(names) != 0 {
			t.Fatal("scope must be empty, not all", names)
		}
	}
	names, err := f.Port.RepositoryScope(t.Context(), []string{"origin"})
	mustRun(t, err)
	if len(names) != 1 || names[0] != "origin/team/repo" {
		t.Fatal("upstream tokens escaped", names)
	}
	for i := range 201 {
		f.Repository(t, fmt.Sprintf("origin/team/repo-%d", i))
	}
	first, err := f.Port.Repositories(t.Context(), stateport.CatalogFilter{Limit: 200})
	mustRun(t, err)
	if len(first) != 200 {
		t.Fatal("catalogue first page missing", len(first))
	}
	second, err := f.Port.Repositories(t.Context(), stateport.CatalogFilter{Limit: 200, Before: first[len(first)-1].ID})
	mustRun(t, err)
	if len(second) != 2 {
		t.Fatal("catalogue cursor lost repositories", len(second))
	}
}
