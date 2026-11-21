package cronsync_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

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
	if err := f.c.AuthorityChanged(t.Context(), publisher); !errors.Is(err, outage) {
		t.Fatal(err)
	}
	if calls != 1 || !f.object(f.repo, "nightly").Spec.Suspend {
		t.Fatal("failed publication resumed schedule")
	}
	if err := f.c.SetPause(t.Context(), f.repo, "nightly"); err == nil {
		t.Fatal("resume cleared authority suspension")
	}
	if err := f.c.AuthorityChanged(t.Context(), publisher); err != nil {
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
			err := f.c.AuthorityChanged(t.Context(), func(context.Context) error { published = true; return nil })
			if !errors.Is(err, outage) || published || !strings.Contains(err.Error(), "some schedules may still run") {
				t.Fatal("unconfirmed publication", err)
			}
		})
	}
}
func TestWideAuthoritySuspendsAndReadmitsAllRepositories(t *testing.T) {
	f := newFixture(t)
	other := f.repo
	other.ID = 8
	other.Qualified = "origin/team/other"
	f.repos = append(f.repos, other)
	f.put(other, "schedules: [{name: nightly, cron: '0 3 * * *'}]")
	f.sync()
	before := len(f.calls)
	if err := f.c.AuthorityChanged(t.Context(), func(ctx context.Context) error {
		if !f.objectAt(ctx, f.repo, "nightly").Spec.Suspend || !f.objectAt(ctx, other, "nightly").Spec.Suspend {
			t.Fatal("published before every schedule was paused")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != before+2 || f.object(f.repo, "nightly").Spec.Suspend || f.object(other, "nightly").Spec.Suspend {
		t.Fatal("wide repair missed a repository", f.calls)
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
		finished <- f.c.AuthorityChanged(t.Context(), func(context.Context) error { close(published); return nil })
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
