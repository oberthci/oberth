package cronsync_test

import (
	"context"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/cronsync"
)

// statusError is the schedule list's error phrase for one entry.
func (f *fixture) statusError(c *cronsync.Syncer, repo cronsync.Repository) string {
	f.t.Helper()
	status, err := c.Status(f.t.Context(), repo, "nightly")
	if err != nil {
		f.t.Fatal(err)
	}
	return status.Error
}

// A full pass, from a notice for a repository the catalogue no longer knows
// or from startup, reconciles every repository, each within its own attempt:
// a slow first repository fails alone and the next still syncs inside the
// caller's shared bound.
func TestFullPassGivesEachRepositoryItsOwnAttempt(t *testing.T) {
	passes := map[string]func(*cronsync.Syncer, context.Context) error{
		"unknown key": func(c *cronsync.Syncer, ctx context.Context) error { return c.RefsChanged(ctx, "origin/team/removed", nil) },
		"startup":     func(c *cronsync.Syncer, ctx context.Context) error { return c.SyncAll(ctx) },
	}
	for name, pass := range passes {
		for _, attempt := range []time.Duration{time.Minute, 0} {
			t.Run(name+map[bool]string{true: "/own attempt", false: "/shared bound only"}[attempt > 0], func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) { slowFirstRepositoryPass(t, attempt, pass) })
			})
		}
	}
}

// slowFirstRepositoryPass runs a full pass under a 90-second bound while the
// first repository's Git never answers.
func slowFirstRepositoryPass(t *testing.T, attempt time.Duration, pass func(*cronsync.Syncer, context.Context) error) {
	f := newFixture(t)
	other := f.otherRepository()
	f.sync()
	config := f.config
	config.RepositoryAttempt = attempt
	c, err := cronsync.New(config)
	if err != nil {
		t.Fatal(err)
	}
	f.gitHook = func(ctx context.Context, verb, name string) error {
		if verb == "resolve" && name == "refs/heads/main" {
			<-ctx.Done()
		}
		return nil
	}
	notice, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	before := len(f.calls)
	_ = pass(c, notice)
	f.gitHook = nil
	synced := slices.ContainsFunc(f.calls[before:], func(call cronsync.Preparation) bool { return call.Repository.ID == other.ID })
	if f.statusError(c, f.repo) == "" || f.statusError(c, other) != "" {
		t.Fatal("the slow repository must fail alone")
	}
	if synced != (attempt > 0) {
		t.Fatal("the other repository's sync depended on the slow one", attempt, synced)
	}
}

// Once the supplied context ends, repositories the pass has not reached keep
// their state instead of being recorded as failed.
func TestPassLeavesUnreachedRepositoriesAsTheyAre(t *testing.T) {
	f := newFixture(t)
	other := f.otherRepository()
	f.sync()
	ended, cancel := context.WithCancel(t.Context())
	f.gitHook = func(ctx context.Context, verb, name string) error {
		if verb == "resolve" && name == "refs/heads/main" {
			cancel()
		}
		return nil
	}
	_ = f.c.RefsChanged(ended, "origin/team/removed", nil)
	f.gitHook = nil
	if got := f.statusError(f.c, other); got != "" {
		t.Fatal("an unreached repository was recorded as failed", got)
	}
}
