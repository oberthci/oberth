package contracttest

import (
	"fmt"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// checkRecentRuns pins the repository strip: the newest RecentRunLimit runs of
// each named repository, oldest first, with their recorded status, ref and time;
// runs of a repository outside the names never appear.
func checkRecentRuns(t *testing.T, f RunFixture) {
	main, quiet, hidden, idle := "origin/team/repo", "origin/team/quiet", "origin/team/hidden", "origin/team/idle"
	for _, name := range []string{quiet, hidden, idle} {
		f.Repository(t, name)
	}
	statuses := []stepmodel.RunStatus{stepmodel.RunPassed, stepmodel.RunFailed, stepmodel.RunCancelled, stepmodel.RunQueued}
	var want []stateport.Run
	for i := range stateport.RecentRunLimit + 2 {
		r := acceptOneRun(t, f, RunCase{ID: fmt.Sprint("main-", i), Ref: fmt.Sprintf("refs/heads/topic-%d", i)})
		if status := statuses[i%len(statuses)]; status != stepmodel.RunQueued {
			mustRun(t, f.Port.Finish(t.Context(), stateport.Action{Actor: r.Origin.Actor}, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: status}))
			r.Status = status
		}
		want = append(want, r)
		acceptOneRun(t, f, RunCase{ID: fmt.Sprint("hidden-", i), Repo: hidden})
	}
	lone := acceptOneRun(t, f, RunCase{ID: "quiet", Repo: quiet})
	got, err := f.Port.RecentRuns(t.Context(), []string{quiet, main, idle})
	mustRun(t, err)
	if len(got) != 2 || len(got[quiet]) != 1 || got[quiet][0].ID != lone.ID || got[idle] != nil || got[hidden] != nil {
		t.Fatal("recent runs crossed repositories or invented an entry", got)
	}
	checkMarks(t, got[main], want[2:])
	for _, names := range [][]string{nil, make([]string, stateport.RunPageLimit+1)} {
		_, err := f.Port.RecentRuns(t.Context(), names)
		runCode(t, err, publicerr.InvalidInput)
	}
	got, err = f.Port.RecentRuns(t.Context(), []string{})
	mustRun(t, err)
	if len(got) != 0 {
		t.Fatal("empty scope widened", got)
	}
}

func checkMarks(t *testing.T, got []stateport.RunMark, want []stateport.Run) {
	t.Helper()
	if len(got) != stateport.RecentRunLimit {
		t.Fatal("recent runs not bounded to the limit", len(got))
	}
	for i, r := range want {
		m := got[i]
		if m.ID != r.ID || m.Status != r.Status || m.Ref != r.Ref || !m.CreatedAt.Equal(r.CreatedAt) {
			t.Fatal("recent run out of order or changed", i, m, r.RunSummary)
		}
	}
}
