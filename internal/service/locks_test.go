package service_test

import (
	"slices"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
)

const (
	staleLock  = "runs/ConfigMap/oberth-sync-limits/heavy-go"
	staleClear = "kubectl -n oberth rollout restart deployment/oberth-controller-workflow-controller"
)

func liveLocks() []service.Lock {
	return []service.Lock{{Repo: repository, RunID: "live", Name: staleLock, Permits: 1}, {Repo: repository, RunID: "queued", Name: staleLock, Waiting: true}}
}

// An ended run the controller still counts is listed apart from holders, with
// a remedy and the exact command that releases it; other repositories stay hidden.
func TestSyncStatusListsStaleHolderWithItsCommand(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.exec.LockValues = append(liveLocks(), service.Lock{Repo: repository, RunID: runID, Name: staleLock, Clear: staleClear, Permits: 1}, service.Lock{Repo: "private/team/hidden", RunID: "hidden", Name: staleLock, Clear: staleClear, Permits: 1})
	locks, err := f.s.SyncStatus(t.Context(), caller())
	if err != nil || len(locks.Locks) != 1 {
		t.Fatalf("lock status %+v %v", locks, err)
	}
	got := locks.Locks[0]
	if !slices.Equal(got.Holders, []apicontract.LockHolder{{RunID: "live", Permits: 1}}) || !slices.Equal(got.Waiters, []string{"queued"}) || !slices.Equal(got.Stale, []string{runID}) || got.Command != staleClear {
		t.Fatalf("stale holder not separated %+v", got)
	}
	if got.Remedy != "A run that ended still blocks this lock; restart the workflow controller to release it." || publicerr.CheckMessage(got.Remedy, 120) != nil {
		t.Fatalf("stale holder remedy %q", got.Remedy)
	}
}

// A run waiting behind another repository's stale unit learns the remedy and
// command without that run's name; a lock the caller has no row on stays unseen.
func TestSyncStatusKeepsRemedyOfAHiddenStaleHolder(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.exec.LockValues = []service.Lock{
		{Repo: repository, RunID: "queued", Name: staleLock, Waiting: true},
		{Repo: "private/team/hidden", RunID: "hidden", Name: staleLock, Clear: staleClear, Permits: 1},
		{Repo: "private/team/hidden", RunID: "other", Name: "runs/Mutex/hidden", Clear: staleClear, Permits: 1},
	}
	locks, err := f.s.SyncStatus(t.Context(), caller())
	if err != nil || len(locks.Locks) != 1 {
		t.Fatalf("lock status %+v %v", locks, err)
	}
	got := locks.Locks[0]
	if got.Lock != staleLock || got.Stale != nil || len(got.Holders) != 0 || got.Remedy == "" || got.Command != staleClear {
		t.Fatalf("hidden stale holder lost its remedy or leaked its run %+v", got)
	}
}

// Live holders and waiters carry no repair.
func TestSyncStatusLiveLocksCarryNoRepair(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.exec.LockValues = liveLocks()
	locks, err := f.s.SyncStatus(t.Context(), caller())
	if err != nil || len(locks.Locks) != 1 {
		t.Fatalf("lock status %+v %v", locks, err)
	}
	if got := locks.Locks[0]; got.Stale != nil || got.Remedy != "" || got.Command != "" || len(got.Holders) != 1 || len(got.Waiters) != 1 {
		t.Fatalf("live locks carry a repair %+v", got)
	}
}
