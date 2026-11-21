package cronsync_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/cronsync"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/schedule"
)

func (f *fixture) authority(ctx context.Context, repo cronsync.Repository, publish func(context.Context) error) error {
	f.t.Helper()
	unsynced, err := f.c.AuthorityChanged(ctx, repo.Qualified, publish)
	if unsynced != nil {
		f.t.Fatal("unexpected unsynced schedules", unsynced)
	}
	return err
}

func TestCronAuthoritySuspendsBeforePublication(t *testing.T) {
	f := newFixture(t)
	f.sync()
	outage := errors.New("publish failed")
	calls := 0
	publisher := func(ctx context.Context) error {
		calls++
		if !f.objectAt(ctx, f.repo, "nightly").Spec.Suspend {
			t.Fatal("published before suspension")
		}
		if calls == 1 {
			return outage
		}
		return nil
	}
	if err := f.authority(t.Context(), f.repo, publisher); !errors.Is(err, outage) {
		t.Fatal(err)
	}
	if calls != 1 || !f.object(f.repo, "nightly").Spec.Suspend {
		t.Fatal("failed publication resumed schedule")
	}
	if err := f.c.SetPause(t.Context(), f.repo, "nightly"); err == nil {
		t.Fatal("resume cleared authority suspension")
	}
	if err := f.authority(t.Context(), f.repo, publisher); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || f.object(f.repo, "nightly").Spec.Suspend {
		t.Fatal("repeated action did not readmit")
	}
}
func TestAuthorityRefusesIncompleteListAndFailedSuspension(t *testing.T) {
	for _, operation := range []string{"list", "update"} {
		t.Run(operation, func(t *testing.T) {
			f := newFixture(t)
			f.sync()
			outage := errors.New("native unavailable")
			f.hook = func(_ context.Context, verb, _ string) error {
				if verb == operation {
					return outage
				}
				return nil
			}
			published := false
			err := f.authority(t.Context(), f.repo, func(context.Context) error { published = true; return nil })
			if !errors.Is(err, outage) || published || !strings.Contains(err.Error(), "some schedules may still run") {
				t.Fatal("unconfirmed publication", err)
			}
		})
	}
}

// otherRepository adds a second repository whose default branch is trunk, so
// a Git hook can fail its reads alone.
func (f *fixture) otherRepository() cronsync.Repository {
	other := cronsync.Repository{ID: 8, Qualified: "origin/team/other", DefaultBranch: "trunk"}
	f.repos = append(f.repos, other)
	ref := gitport.Ref{Name: "refs/heads/trunk", ObjectSHA: strings.Repeat("d", 40), CommitSHA: strings.Repeat("d", 40)}
	f.git.PutRef(other.Qualified, ref)
	f.git.PutFile(gitport.Revision{Repository: other.Qualified, Commit: ref.CommitSHA}, gitport.File{Path: schedule.FileName, Mode: 0100644, Blob: strings.Repeat("e", 40), Bytes: []byte("schedules: [{name: nightly, cron: '0 3 * * *'}]")})
	return other
}

func TestAuthorityPausesAndReadmitsOnlyItsRepository(t *testing.T) {
	f := newFixture(t)
	other := f.otherRepository()
	f.sync()
	unreadable := &gitport.UnavailableError{Op: "forge"}
	f.gitHook = func(_ context.Context, verb, name string) error {
		if verb == "resolve" && name == "refs/heads/trunk" {
			return unreadable
		}
		return nil
	}
	before := len(f.calls)
	unsynced, err := f.c.AuthorityChanged(t.Context(), f.repo.Qualified, func(ctx context.Context) error {
		if !f.objectAt(ctx, f.repo, "nightly").Spec.Suspend || f.objectAt(ctx, other, "nightly").Spec.Suspend {
			t.Fatal("the barrier must pause the grant's repository and no other")
		}
		return nil
	})
	if err != nil || unsynced != nil {
		t.Fatal("another repository's unreadable schedule decided the outcome", err, unsynced)
	}
	if len(f.calls) != before+1 || f.calls[before].Repository.ID != f.repo.ID {
		t.Fatal("re-admission reached beyond the grant's repository", f.calls[before:])
	}
	if f.object(f.repo, "nightly").Spec.Suspend || f.object(other, "nightly").Spec.Suspend {
		t.Fatal("a schedule stayed paused")
	}
}

func TestAuthorityPublishedReportsUnsyncedSchedulesSeparately(t *testing.T) {
	f := newFixture(t)
	other := f.otherRepository()
	f.sync()
	unreadable := &gitport.UnavailableError{Op: "forge"}
	f.gitHook = func(_ context.Context, verb, name string) error {
		if verb == "resolve" && name == "refs/heads/trunk" {
			return unreadable
		}
		return nil
	}
	published := false
	unsynced, err := f.c.AuthorityChanged(t.Context(), other.Qualified, func(context.Context) error { published = true; return nil })
	if err != nil || !published {
		t.Fatal("a published change reported failure", err)
	}
	if !errors.Is(unsynced, unreadable) || !f.object(other, "nightly").Spec.Suspend {
		t.Fatal("unsynced schedules must be reported and stay paused", unsynced)
	}
	if f.object(f.repo, "nightly").Spec.Suspend {
		t.Fatal("another repository was paused")
	}
}

func TestAuthorityForUnregisteredRepositoryPublishesWithoutBarrier(t *testing.T) {
	f := newFixture(t)
	f.sync()
	published := false
	if err := f.authority(t.Context(), cronsync.Repository{Qualified: "origin/team/absent"}, func(context.Context) error { published = true; return nil }); err != nil || !published {
		t.Fatal("unregistered repository blocked publication", err)
	}
	if f.object(f.repo, "nightly").Spec.Suspend {
		t.Fatal("unregistered repository paused another one")
	}
}

func TestAuthorityPublicationWaitsForBlockedNativeSuspension(t *testing.T) {
	f := newFixture(t)
	f.sync()
	entered, release, published := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var block, cleanup sync.Once
	t.Cleanup(func() { cleanup.Do(func() { close(release) }) })
	f.hook = func(ctx context.Context, verb, _ string) error {
		if verb == "update" {
			block.Do(func() {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
		}
		return ctx.Err()
	}
	finished := make(chan error, 1)
	go func() {
		_, err := f.c.AuthorityChanged(t.Context(), f.repo.Qualified, func(context.Context) error { close(published); return nil })
		finished <- err
	}()
	<-entered
	select {
	case <-published:
		t.Fatal("publication bypassed blocked native suspension")
	default:
	}
	cleanup.Do(func() { close(release) })
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	select {
	case <-published:
	default:
		t.Fatal("authority was not published after confirmed suspension")
	}
}
