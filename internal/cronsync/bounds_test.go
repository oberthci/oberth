package cronsync_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/cronsync"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/schedule"
)

func TestConstructorRequiresAllProductionReaders(t *testing.T) {
	f := newFixture(t)
	for _, remove := range []func(*cronsync.Config){
		func(c *cronsync.Config) { c.Repository = nil },
		func(c *cronsync.Config) { c.Repositories = nil },
		func(c *cronsync.Config) { c.Client = nil },
		func(c *cronsync.Config) { c.Git = nil },
		func(c *cronsync.Config) { c.Admit = nil },
		func(c *cronsync.Config) { c.PauseIntent = nil },
		func(c *cronsync.Config) { c.Now = nil },
		func(c *cronsync.Config) { c.ValidateNativeSchema = nil },
		func(c *cronsync.Config) { c.Namespace = "" },
	} {
		config := f.config
		remove(&config)
		if _, err := cronsync.New(config); err == nil {
			t.Fatal("production dependency became optional")
		}
	}
}

func TestRelevantRefReadmitsEveryEntry(t *testing.T) {
	f := newFixture(t)
	f.put(f.repo, "schedules: [{name: nightly, cron: '0 3 * * *'}, {name: other, cron: '0 4 * * *', branch: work}]")
	ref := gitport.Ref{Name: "refs/heads/work", ObjectSHA: strings.Repeat("b", 40), CommitSHA: strings.Repeat("b", 40)}
	f.git.PutRef(f.repo.Qualified, ref)
	f.sync()
	first := f.object(f.repo, "nightly")
	before := len(f.calls)
	ref.ObjectSHA = strings.Repeat("c", 40)
	ref.CommitSHA = ref.ObjectSHA
	f.git.PutRef(f.repo.Qualified, ref)
	if err := f.c.RefsChanged(t.Context(), f.repo.Qualified, []string{ref.Name}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != before+2 || f.calls[len(f.calls)-1].Entry.Name != "other" || !reflect.DeepEqual(f.object(f.repo, "nightly"), first) {
		t.Fatal("unrelated entry changed")
	}
}
func TestRepositoryArchiveAndRemovalOrphanOnlyItsParents(t *testing.T) {
	for _, removed := range []bool{false, true} {
		t.Run(map[bool]string{true: "removed", false: "archived"}[removed], func(t *testing.T) {
			f := newFixture(t)
			other := f.repo
			other.ID = 8
			other.Qualified = "origin/team/other"
			f.repos = append(f.repos, other)
			f.put(other, "schedules: [{name: nightly, cron: '0 3 * * *'}]")
			f.sync()
			first := f.object(f.repo, "nightly")
			otherFirst := f.object(other, "nightly")
			if removed {
				f.repos = f.repos[1:]
			} else {
				f.repos[0].Archived = true
			}
			if err := f.c.RefsChanged(t.Context(), f.repo.Qualified, nil); err != nil {
				t.Fatal(err)
			}
			actions := f.native.Actions()
			last := actions[len(actions)-1]
			assertOrphanDeletion(t, last, first)
			if !reflect.DeepEqual(f.object(other, "nightly"), otherFirst) {
				t.Fatal("unrelated repository changed")
			}
		})
	}
}
func TestOrdinaryLookupFailureDoesNotWidenScope(t *testing.T) {
	f := newFixture(t)
	f.sync()
	f.catalogErr = errors.New("catalog unavailable")
	if err := f.c.RefsChanged(t.Context(), f.repo.Qualified, []string{"refs/heads/main"}); !errors.Is(err, f.catalogErr) {
		t.Fatal(err)
	}
}
func TestSuspensionHasBoundedMessagesAndUncutDetail(t *testing.T) {
	f := newFixture(t)
	f.sync()
	failure := publicerr.New(publicerr.InvalidInput, "pipeline is invalid", "Correct the pipeline.", nil)
	failure.Detail = strings.Repeat("full public diagnostic\n", 300)
	f.admitErr = failure
	f.sync()
	status, err := f.c.Status(t.Context(), f.repo, "nightly")
	if err != nil || len(status.Error) > 40 || !strings.Contains(status.Detail, failure.Detail) {
		t.Fatal("full diagnostic was cut", status, err)
	}
}

func TestMixedFileAbsenceDoesNotDeleteNativeParent(t *testing.T) {
	f := newFixture(t)
	f.sync()
	f.gitHook = func(_ context.Context, verb, _ string) error {
		if verb == "read" {
			return errors.Join(&gitport.AbsentError{Path: schedule.FileName}, authorError())
		}
		return nil
	}
	f.sync()
	if !f.object(f.repo, "nightly").Spec.Suspend {
		t.Fatal("mixed absence deleted or activated parent")
	}
}

func TestConstructorRefusesInvalidOperatorLimits(t *testing.T) {
	f := newFixture(t)
	for _, limits := range []schedule.Limits{{MaxEntries: -1}, {MaxEntries: 257}} {
		config := f.config
		config.Limits = limits
		if _, err := cronsync.New(config); err == nil {
			t.Fatal("invalid operator limits admitted", limits)
		}
	}
	if len(f.native.Actions()) != 0 {
		t.Fatal("constructor performed native I/O")
	}
}

func TestMissingOldRepositoryKeyKeepsCurrentDurableID(t *testing.T) {
	f := newFixture(t)
	f.sync()
	before := f.object(f.repo, "nightly")
	renamed := f.repo
	renamed.Qualified = "origin/team/renamed"
	f.repos = []cronsync.Repository{renamed}
	f.put(renamed, "schedules: [{name: nightly, cron: '0 3 * * *'}]")
	if err := f.c.RefsChanged(t.Context(), f.repo.Qualified, nil); err != nil {
		t.Fatal(err)
	}
	after := f.object(renamed, "nightly")
	if after.UID != before.UID || after.Spec.Suspend || f.calls[len(f.calls)-1].Repository.Qualified != renamed.Qualified {
		t.Fatal("stale key deleted or skipped the current repository")
	}
	for _, action := range f.native.Actions() {
		if action.Verb == "delete" {
			t.Fatal("stale key authorized deletion")
		}
	}
}
