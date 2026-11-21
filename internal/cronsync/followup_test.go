package cronsync_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/cronsync"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/schedule"
)

func TestUnreadableDeclarationStillSyncsAfterRelevancePreflight(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		t.Run(map[bool]string{false: "unavailable", true: "recovered"}[recovered], func(t *testing.T) {
			assertRelevanceSync(t, recovered)
		})
	}
}
func assertRelevanceSync(t *testing.T, recovered bool) {
	t.Helper()
	f := newFixture(t)
	f.sync()
	reads, lists, admitted := 0, 0, len(f.calls)
	outage := errors.New("Git read unavailable")
	f.gitHook = func(_ context.Context, verb, _ string) error {
		if verb == "read" {
			reads++
			if reads == 1 || !recovered {
				return outage
			}
		}
		return nil
	}
	f.hook = func(_ context.Context, verb, _ string) error {
		if verb == "list" {
			lists++
		}
		return nil
	}
	err := f.c.RefsChanged(t.Context(), f.repo.Qualified, []string{"refs/heads/unrelated"})
	if errors.Is(err, outage) == recovered || reads != 2 || lists != 1 {
		t.Fatal("unreadable preflight skipped synchronization", reads, lists, err)
	}
	if recovered && len(f.calls) != admitted+1 {
		t.Fatal("recovered declaration was not admitted")
	}
}

func TestSnapshotUsesSuppliedRepositoryWithoutCatalogueReads(t *testing.T) {
	f := newFixture(t)
	f.put(f.repo, "schedules: [{name: nightly, cron: '0 3 * * *'}, {name: other, cron: '0 4 * * *'}]")
	f.sync()
	config := f.config
	catalogs, pauses := 0, 0
	config.Repositories = func(context.Context) ([]cronsync.Repository, error) {
		catalogs++
		return nil, errors.New("catalog unavailable")
	}
	config.PauseIntent = func(_ context.Context, repo, entry string) (bool, error) {
		if repo != f.repo.Qualified {
			t.Fatal("pause intent read for another repository", repo)
		}
		pauses++
		return entry == "other", nil
	}
	c, err := cronsync.New(config)
	if err != nil {
		t.Fatal(err)
	}
	statuses, err := c.Snapshot(t.Context(), f.repo)
	if err != nil || catalogs != 0 || pauses != 2 || len(statuses) != 2 {
		t.Fatal("snapshot repeated repository lookup", catalogs, pauses, statuses, err)
	}
	if statuses[0].Entry != "nightly" || statuses[0].UserPaused || statuses[1].Entry != "other" || !statuses[1].UserPaused {
		t.Fatal("entry intent or sort order lost", statuses)
	}
}

func TestSetPauseReadsAuditedIntentOnce(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "present"}[present], func(t *testing.T) {
			f := newFixture(t)
			if present {
				f.sync()
			}
			config := f.config
			reads := 0
			config.PauseIntent = func(context.Context, string, string) (bool, error) { reads++; return true, nil }
			c, err := cronsync.New(config)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.SetPause(t.Context(), f.repo, "nightly"); err != nil || reads != 1 {
				t.Fatal("pause did not read intent exactly once", reads, err)
			}
			if present && !f.object(f.repo, "nightly").Spec.Suspend {
				t.Fatal("pause intent was discarded")
			}
			if !present && len(f.native.Actions()) != 0 {
				t.Fatal("saved pause created a parent")
			}
		})
	}
}

func TestSuspensionDoesNotReadPauseIntentOrCatalogue(t *testing.T) {
	for _, authority := range []bool{false, true} {
		t.Run(map[bool]string{false: "refusal", true: "authority"}[authority], func(t *testing.T) {
			f := newFixture(t)
			f.sync()
			f.admitErr = authorError()
			config := f.config
			catalogs, pauses := 0, 0
			config.Repositories = func(ctx context.Context) ([]cronsync.Repository, error) { catalogs++; return f.repositories(ctx) }
			config.PauseIntent = func(context.Context, string, string) (bool, error) {
				pauses++
				return false, errors.New("audit unavailable")
			}
			c, err := cronsync.New(config)
			if err != nil {
				t.Fatal(err)
			}
			listings := 1
			if authority {
				listings = 0
				var unsynced error
				unsynced, err = c.AuthorityChanged(t.Context(), f.repo.Qualified, func(ctx context.Context) error {
					if catalogs != 0 || pauses != 0 || !f.objectAt(ctx, f.repo, "nightly").Spec.Suspend {
						t.Fatal("fence read intent or published before suspension", catalogs, pauses)
					}
					return nil
				})
				err = errors.Join(err, unsynced)
			} else {
				err = c.SyncAll(t.Context())
			}
			if err != nil || pauses != 0 || catalogs != listings || !f.object(f.repo, "nightly").Spec.Suspend {
				t.Fatal("unreadable intent blocked unconditional suspension", catalogs, pauses, err)
			}
		})
	}
}

func TestReenteringCallbackReturnsItsDeadlineError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		callbacks := 0
		f.admitHook = func(ctx context.Context, _ cronsync.Preparation) error {
			callbacks++
			inner, cancel := context.WithTimeout(ctx, time.Millisecond)
			defer cancel()
			if err := f.c.SyncRepository(inner, f.repo.Qualified); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("reentry did not respect its context", err)
			}
			return nil
		}
		f.sync()
		if callbacks != 1 || f.object(f.repo, "nightly").Spec.Suspend {
			t.Fatal("callback reentry performed another sync or damaged the owner")
		}
	})
}

func TestWrappedMixedAbsenceCannotDeleteParent(t *testing.T) {
	f := newFixture(t)
	f.sync()
	outage := errors.New("store unavailable")
	f.gitHook = func(_ context.Context, verb, _ string) error {
		if verb == "read" {
			return fmt.Errorf("wrapped: %w", errors.Join(&gitport.AbsentError{Path: schedule.FileName}, outage))
		}
		return nil
	}
	if err := f.c.SyncAll(t.Context()); !errors.Is(err, outage) {
		t.Fatal("mixed absence authorized deletion", err)
	}
	if f.object(f.repo, "nightly").Spec.Suspend {
		t.Fatal("mixed absence suspended the last admitted spec")
	}
	for _, action := range f.native.Actions() {
		if action.Verb == "delete" {
			t.Fatal("mixed absence deleted the parent")
		}
	}
}

func TestFullSyncRepairClearsResumeBlock(t *testing.T) {
	f := newFixture(t)
	f.paused = true
	f.sync()
	f.catalogErr = errors.New("catalog unavailable")
	if err := f.c.SyncAll(t.Context()); !errors.Is(err, f.catalogErr) {
		t.Fatal(err)
	}
	f.catalogErr = nil
	f.sync()
	f.paused = false
	if err := f.c.SetPause(t.Context(), f.repo, "nightly"); err != nil || f.object(f.repo, "nightly").Spec.Suspend {
		t.Fatal("successful full sync left resume blocked", err)
	}
}
