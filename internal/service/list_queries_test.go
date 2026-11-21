package service_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/service/contracttest"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestRepositoryFilterReadsNoGitAndDetailReadsOnlySelectedRepository(t *testing.T) {
	f := setup(t)
	f.store.native.SeedRepository(stateport.Repository{ID: 2, Upstream: "origin", Name: "other", Canonical: "origin/team/other"})
	var read []string
	f.c.Git = contracttest.GitFuncs{ListRefsFunc: func(_ context.Context, repo string) ([]gitport.Ref, error) {
		read = append(read, repo)
		return nil, nil
	}}
	f.source.Default = func(_ context.Context, repo string) (string, error) {
		read = append(read, repo)
		return "refs/heads/main", nil
	}
	f.restart(t)
	call, err := f.s.PrepareRepoList(t.Context(), workerCaller(), apicontract.RepoListArgs{NamesOnly: true})
	page, err := execute(t.Context(), call, err)
	if err != nil || len(page.Repositories) != 2 || len(read) != 0 || f.store.Calls()["Runs"] != 0 {
		t.Fatal(page, read, err)
	}
	call, err = f.s.PrepareRepoList(t.Context(), workerCaller(), apicontract.RepoListArgs{Repo: repository})
	page, err = execute(t.Context(), call, err)
	if err != nil || len(page.Repositories) != 1 || len(read) != 2 || read[0] != repository || read[1] != repository {
		t.Fatal(page, read, err)
	}
}

func TestStoreCountsAllStatusesWithinCallerScope(t *testing.T) {
	p := people(t)
	seedListMeasurement(t, p)
	db, err := sql.Open("sqlite", p.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.ExecContext(t.Context(), `UPDATE runs SET status=CASE sequence%5 WHEN 0 THEN 'queued' WHEN 1 THEN 'running' WHEN 2 THEN 'passed' WHEN 3 THEN 'failed' ELSE 'cancelled' END`)
	if err != nil {
		t.Fatal(err)
	}
	p.f.c.Runs = p.state
	p.f.c.Catalog = p.state
	p.f.restart(t)
	call, err := p.f.s.Views().Counts.Handler.Prepare(t.Context(), workerCaller(), apicontract.Empty{})
	counts, err := execute(t.Context(), call, err)
	if err != nil || counts != (apicontract.RunCounts{Queued: 4000, Running: 4000, Passed: 4000, Failed: 4000, Cancelled: 4000}) {
		t.Fatal(counts, err)
	}
	c := workerCaller()
	c.Actor.Scope = []string{"hidden"}
	p.f.store.Actors[c.Actor.Fingerprint] = c.Actor
	call, err = p.f.s.Views().Counts.Handler.Prepare(t.Context(), c, apicontract.Empty{})
	counts, err = execute(t.Context(), call, err)
	if err != nil || counts != (apicontract.RunCounts{}) {
		t.Fatal("hidden runs counted", counts, err)
	}
}

func TestArchiveFilterPrecedesRepositoryGitReads(t *testing.T) {
	f := setup(t)
	archived := "origin/team/archived"
	f.store.native.SeedRepository(stateport.Repository{ID: 2, Upstream: "origin", Name: "archived", Canonical: archived, Archived: true})
	reads := make(chan string, 8)
	f.c.Git = contracttest.GitFuncs{ListRefsFunc: func(_ context.Context, repo string) ([]gitport.Ref, error) { reads <- repo; return nil, nil }}
	f.source.Default = func(_ context.Context, repo string) (string, error) { reads <- repo; return "refs/heads/main", nil }
	f.restart(t)
	for _, tc := range []struct{ mode, repo string }{{"", repository}, {"only", archived}} {
		call, err := f.s.PrepareRepoList(t.Context(), workerCaller(), apicontract.RepoListArgs{Archived: tc.mode})
		page, err := execute(t.Context(), call, err)
		if err != nil || len(page.Repositories) != 1 || page.Repositories[0].Repo != tc.repo {
			t.Fatal(page, err)
		}
		if len(reads) != 2 {
			t.Fatal("Git read excluded repositories", tc.mode, len(reads))
		}
		for range 2 {
			if got := <-reads; got != tc.repo {
				t.Fatal("Git read hidden repository", got)
			}
		}
	}
}
