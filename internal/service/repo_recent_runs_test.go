package service_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

const hiddenRepository = "hidden/team/secret"

// TestRepoListReadsEveryStripInOneStoreCall: the repository list carries each
// visible repository's last runs from one store read, however many repositories
// it shows, and a caller sees only the runs the run list would show it.
func TestRepoListReadsEveryStripInOneStoreCall(t *testing.T) {
	f := setup(t)
	names, widget := seedStrips(f)
	page := listRepos(t, f, workerCaller(), true)
	if f.store.Calls()["RecentRuns"] != 1 || f.store.Calls()["Runs"] != 1 || len(page.Repositories) != len(names) {
		t.Fatal("repository strips not read in one store call", f.store.Calls(), len(page.Repositories))
	}
	strips := map[string][]stateport.RunMark{}
	for _, r := range page.Repositories {
		strips[r.Repo] = r.RecentRuns
	}
	checkStrip(t, strips[repository], widget[3:])
	if one := strips[names[1]]; len(one) != 1 || one[0].Ref != "refs/heads/fix" || one[0].Status != stepmodel.RunFailed {
		t.Fatal("single run strip wrong", one)
	}
	if _, listed := strips[hiddenRepository]; listed || strips[names[2]] != nil {
		t.Fatal("hidden repository listed or a repository without runs has a strip", strips)
	}
}

// TestRepoListStripOnlyWhenAsked: a plain repository list reads no strip, and
// an unscoped caller sees the strip of a repository the worker cannot see.
func TestRepoListStripOnlyWhenAsked(t *testing.T) {
	f := setup(t)
	seedStrips(f)
	if page := listRepos(t, f, workerCaller(), false); f.store.Calls()["RecentRuns"] != 0 || page.Repositories[0].RecentRuns != nil {
		t.Fatal("strip read although not asked for", f.store.Calls(), page.Repositories[0])
	}
	seen := 0
	for _, r := range listRepos(t, f, apicontract.Caller{Actor: admin()}, true).Repositories {
		if r.Repo == hiddenRepository {
			seen++
			if len(r.RecentRuns) != stateport.RecentRunLimit {
				t.Fatal("unscoped caller lost a repository strip", r.RecentRuns)
			}
		}
	}
	if seen != 1 {
		t.Fatal("unscoped caller did not see the repository hidden from the worker", seen)
	}
}

// seedStrips registers twelve visible repositories and one the worker cannot
// see; the widget repository gets three runs more than a strip holds.
func seedStrips(f *fixture) ([]string, []stateport.RunMark) {
	names := []string{repository}
	for i := range 11 {
		names = append(names, fmt.Sprintf("origin/team/repo%02d", i))
		f.store.native.SeedRepository(stateport.Repository{ID: int64(i + 2), Upstream: "origin", Name: fmt.Sprintf("repo%02d", i), Canonical: names[i+1]})
	}
	f.store.native.SeedRepository(stateport.Repository{ID: 99, Upstream: "hidden", Name: "secret", Canonical: hiddenRepository})
	statuses := []stepmodel.RunStatus{stepmodel.RunPassed, stepmodel.RunFailed, stepmodel.RunRunning, stepmodel.RunQueued, stepmodel.RunCancelled}
	var widget []stateport.RunMark
	for i := range stateport.RecentRunLimit + 3 {
		m := stateport.RunMark{ID: fmt.Sprintf("%032x", i+1), Ref: "refs/heads/main", Status: statuses[i%len(statuses)], CreatedAt: at().Add(time.Duration(i) * time.Minute)}
		widget = append(widget, m)
		f.store.native.SeedRun(stateport.Run{RunSummary: stepmodel.RunSummary{ID: m.ID, Repo: repository, Ref: m.Ref, Status: m.Status, CreatedAt: m.CreatedAt}})
		f.store.native.SeedRun(stateport.Run{RunSummary: stepmodel.RunSummary{ID: fmt.Sprintf("%032x", 1000+i), Repo: hiddenRepository, Ref: "refs/heads/main", Status: stepmodel.RunFailed, CreatedAt: at()}})
	}
	f.store.native.SeedRun(stateport.Run{RunSummary: stepmodel.RunSummary{ID: fmt.Sprintf("%032x", 500), Repo: names[1], Ref: "refs/heads/fix", Status: stepmodel.RunFailed, CreatedAt: at()}})
	return names, widget
}

func listRepos(t *testing.T, f *fixture, c apicontract.Caller, recent bool) apicontract.RepoPage {
	t.Helper()
	call, err := f.s.PrepareRepoList(t.Context(), c, apicontract.RepoListArgs{RecentRuns: recent})
	page, err := execute(t.Context(), call, err)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func checkStrip(t *testing.T, got, want []stateport.RunMark) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatal("strip not bounded", len(got))
	}
	for i := range got {
		if got[i].ID != want[i].ID || got[i].Status != want[i].Status || got[i].Ref != want[i].Ref || !got[i].CreatedAt.Equal(want[i].CreatedAt) {
			t.Fatal("strip not oldest first or changed", i, got[i], want[i])
		}
	}
}
