package cronsync_test

import (
	"context"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/cronsync"
)

// A full pass that cannot read the catalogue or list the native schedules,
// at its start or for its final prune, says so; a pass in which a repository
// fails did reach the others and does not.
func TestFullPassSaysWhenItCannotReadTheCatalogueOrTheSchedules(t *testing.T) {
	f := newFixture(t)
	f.sync()
	f.catalogErr = errors.New("catalogue unavailable")
	if err := f.c.SyncAll(t.Context()); !cronsync.Unreadable(err) || !errors.Is(err, f.catalogErr) {
		t.Fatal("an unreadable catalogue was not an unreadable pass", err)
	}
	f.catalogErr = nil
	listing := errors.New("native unavailable")
	lists := 0
	for _, failing := range []int{1, 3} {
		f.hook = func(_ context.Context, verb, _ string) error {
			if verb == "list" {
				lists++
				if lists == failing {
					return listing
				}
			}
			return nil
		}
		lists = 0
		// List 1 is the pass's start; list 3 is its final prune, after the
		// repository's own step (list 2).
		if err := f.c.SyncAll(t.Context()); !cronsync.Unreadable(err) || !errors.Is(err, listing) {
			t.Fatal("an unreadable native list was not an unreadable pass", failing, err)
		}
	}
	f.hook = nil
	f.gitHook = func(context.Context, string, string) error { return errors.New("git unavailable") }
	if err := f.c.SyncAll(t.Context()); err == nil || cronsync.Unreadable(err) {
		t.Fatal("a repository's own failure was taken for an unreadable pass", err)
	}
}
