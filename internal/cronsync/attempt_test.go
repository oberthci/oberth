package cronsync_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/cronsync"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/schedule"
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

// The startup pass holds the lock for one repository at a time: a
// reconciliation asked for while the pass is on a slow repository is served
// as soon as that repository is done, not after the whole pass. Two slow
// repositories of one minute each; the waiting call has 70 seconds.
func TestStartupPassServesOtherCallsBetweenRepositories(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		other := f.otherRepository()
		third := cronsync.Repository{ID: 9, Qualified: "origin/team/third", DefaultBranch: "slow"}
		f.repos = append(f.repos, third)
		ref := gitport.Ref{Name: "refs/heads/slow", ObjectSHA: strings.Repeat("f", 40), CommitSHA: strings.Repeat("f", 40)}
		f.git.PutRef(third.Qualified, ref)
		f.git.PutFile(gitport.Revision{Repository: third.Qualified, Commit: ref.CommitSHA}, gitport.File{Path: schedule.FileName, Mode: 0100644, Blob: strings.Repeat("e", 40), Bytes: []byte("schedules: [{name: nightly, cron: '0 3 * * *'}]")})
		f.sync()
		// The first and the third repository each take a minute, once.
		slow := map[string]bool{"refs/heads/main": true, "refs/heads/slow": true}
		entered := make(chan struct{}, 2)
		f.gitHook = func(ctx context.Context, verb, name string) error {
			if verb != "resolve" || !slow[name] {
				return nil
			}
			delete(slow, name)
			entered <- struct{}{}
			select {
			case <-time.After(time.Minute):
			case <-ctx.Done():
			}
			return nil
		}
		pass := make(chan error, 1)
		go func() { pass <- f.c.SyncAll(t.Context()) }()
		<-entered
		call, cancel := context.WithTimeout(t.Context(), 70*time.Second)
		defer cancel()
		if err := f.c.SyncRepository(call, other.Qualified); err != nil {
			t.Fatal("a call during the startup pass waited for the whole pass", err)
		}
		if err := <-pass; err != nil {
			t.Fatal(err)
		}
	})
}

// Each startup step reads its repository's current record: a repository
// removed, replaced under a new ID or archived while the pass is on an
// earlier one is not admitted from the record the pass started with, and its
// old parent goes.
func TestStartupStepUsesTheCurrentRepositoryRecord(t *testing.T) {
	changes := map[string]func(cronsync.Repository) []cronsync.Repository{
		"removed":  func(cronsync.Repository) []cronsync.Repository { return nil },
		"replaced": func(r cronsync.Repository) []cronsync.Repository { r.ID = 18; return []cronsync.Repository{r} },
		"archived": func(r cronsync.Repository) []cronsync.Repository { r.Archived = true; return []cronsync.Repository{r} },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			other := f.otherRepository()
			f.sync()
			old := f.object(other, "nightly")
			before := len(f.calls)
			f.gitHook = func(_ context.Context, verb, ref string) error {
				if verb == "resolve" && ref == "refs/heads/main" {
					f.mu.Lock()
					f.repos = append([]cronsync.Repository{f.repo}, change(other)...)
					f.mu.Unlock()
				}
				return nil
			}
			if err := f.c.SyncAll(t.Context()); err != nil {
				t.Fatal(err)
			}
			if slices.ContainsFunc(f.calls[before:], func(call cronsync.Preparation) bool { return call.Repository.ID == other.ID }) {
				t.Fatal("a step admitted the repository from the record the pass started with")
			}
			for _, action := range f.native.Actions() {
				if action.Verb == "delete" && action.Name == old.Name {
					return
				}
			}
			t.Fatal("the stale parent was kept", f.native.Actions())
		})
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
