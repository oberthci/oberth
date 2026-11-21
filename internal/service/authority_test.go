package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestLiveAuthorityOnEveryCall(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	for _, name := range []string{"missing identity", "missing method", "revoked", "changed identity", "scope shrank"} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			f.seed(stepmodel.RunRunning)
			a := caller()
			switch name {
			case "missing identity":
				a.Identity = ""
			case "missing method":
				a.AuthMethod = ""
			case "revoked":
				delete(f.store.Actors, a.Fingerprint)
			case "changed identity":
				current := a
				current.Identity = "other"
				f.store.Actors[a.Fingerprint] = current
			case "scope shrank":
				current := a
				current.Scope = []string{"other"}
				f.store.Actors[a.Fingerprint] = current
			}
			_, err := f.s.RunGet(t.Context(), a, viewQuery{ID: r.ID})
			if err == nil {
				t.Fatal("authority change ignored")
			}
		})
	}
}

func TestPurgeNeedsUnscopedAdministrator(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"agent", "scoped administrator"} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			f.seed(stepmodel.RunFailed)
			a := caller()
			if name == "scoped administrator" {
				a.Admin = true
				f.store.Actors[a.Fingerprint] = a
			}
			_, err := f.s.Purge(t.Context(), a, actionInput{ID: runID, What: "logs", Reason: "space"})
			requireCode(t, err, publicerr.Forbidden)
		})
	}
}

func TestSelectorsHideOutOfScopeRunsAndRepositories(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	r.Repo = "private/team/widget"
	f.store.native.SeedRun(r)
	for _, q := range []viewQuery{{ID: r.ID}, {SHA: head}, {Ref: r.Ref}, {ID: r.ID, Repo: "private/team/widget"}} {
		_, err := f.s.Status(t.Context(), caller(), q)
		requireCode(t, err, publicerr.NotFound)
	}
}
func TestPrepareRejectsHiddenTargetBeforeEffect(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	r.Repo = "private/team/widget"
	f.store.native.SeedRun(r)
	call, err := f.s.PrepareLogs(t.Context(), caller(), logRequest{viewQuery: viewQuery{ID: r.ID}})
	requireCode(t, err, publicerr.NotFound)
	if call != nil {
		t.Fatal("hidden target prepared")
	}
}
func TestRepositoryScopeGuard(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.store.native.SeedRepository(stateport.Repository{Canonical: "private/team/hidden"})
	rows, err := f.s.Repos(t.Context(), caller())
	if err != nil || len(rows) != 1 || rows[0].Repo != repository {
		t.Fatalf("repos %+v %v", rows, err)
	}
}
func TestCurrentLookupFailureIsNotAbsence(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.store.Err = errors.New("authority unavailable")
	_, err := f.s.Status(t.Context(), caller(), viewQuery{ID: runID})
	if !errors.Is(err, f.store.Err) {
		t.Fatal(err)
	}
}
func TestConstructorRejectsMissingDependencies(t *testing.T) {
	t.Parallel()
	f := setup(t)
	for _, change := range []func(*service.Config){func(c *service.Config) { c.Runs = nil }, func(c *service.Config) { c.Starter = nil }, func(c *service.Config) { c.Now = nil }, func(c *service.Config) { c.Authority = nil }, func(c *service.Config) { c.Catalog = nil }, func(c *service.Config) { c.Events = nil }, func(c *service.Config) { c.Git = nil },
		func(c *service.Config) { c.Source = nil }, func(c *service.Config) { c.Exec = nil }, func(c *service.Config) { c.Logs = nil }} {
		c := f.c
		change(&c)
		_, err := service.New(t.Context(), c)
		requireCode(t, err, publicerr.Unavailable)
	}
}
func TestCanceledCallDoesNotReadAuthority(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := f.s.Status(ctx, actor.Actor{}, viewQuery{})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if f.store.Calls()["Current"] != 0 {
		t.Fatal("read canceled authority")
	}
}

func TestSyncStatusHidesOtherRepositories(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.exec.LockValues = []service.Lock{{Repo: repository, RunID: runID, Name: "build"}, {Repo: "private/team/hidden", Name: "secret"}}
	locks, err := f.s.SyncStatus(t.Context(), caller())
	if err != nil || len(locks.Locks) != 1 || locks.Locks[0].Lock != "build" {
		t.Fatalf("lock scope %+v %v", locks, err)
	}
}
