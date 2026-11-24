package service_test

import (
	"github.com/oberthci/oberth/internal/apicontract"
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/service"
)

func TestSyncStatusReportsPermitsAndScopedSteps(t *testing.T) {
	f := setup(t)
	f.exec.LockValues = []service.Lock{
		{Repo: repository, RunID: "build", Name: staleLock, Permits: 2, Limit: 2, Steps: "compile, test"},
	}
	status, err := f.s.SyncStatus(t.Context(), caller())
	if err != nil || len(status.Locks) != 1 {
		t.Fatal(status, err)
	}
	lock := status.Locks[0]
	limit := 2
	want := apicontract.LockState{Lock: staleLock, Held: 2, Limit: &limit, Waiters: []string{}, Holders: []apicontract.LockHolder{{RunID: "build", Permits: 2, Steps: "compile, test"}}}
	if !reflect.DeepEqual(lock, want) {
		t.Fatal("full lock looks like a leak", lock)
	}
	f.exec.LockValues = []service.Lock{
		{Repo: repository, RunID: "queued", Name: staleLock, Waiting: true, Limit: 2},
		{Repo: "private/team/hidden", RunID: "hidden", Name: staleLock, Permits: 2, Limit: 2, Steps: "private step"},
	}
	status, err = f.s.SyncStatus(t.Context(), caller())
	if err != nil || len(status.Locks) != 1 || status.Locks[0].Held != 2 || status.Locks[0].Limit == nil || *status.Locks[0].Limit != 2 || len(status.Locks[0].Holders) != 0 {
		t.Fatal("hidden run leaked or global usage lost", status, err)
	}
}
