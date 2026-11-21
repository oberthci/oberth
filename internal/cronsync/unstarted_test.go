package cronsync_test

import (
	"context"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/cronsync"
)

// A full pass that cannot read the catalogue or list the native schedules
// reached no repository and says so; a pass in which a repository fails did
// reach the others and does not.
func TestFullPassSaysWhenItReachedNoRepository(t *testing.T) {
	f := newFixture(t)
	f.sync()
	f.catalogErr = errors.New("catalogue unavailable")
	if err := f.c.SyncAll(t.Context()); !cronsync.Unstarted(err) || !errors.Is(err, f.catalogErr) {
		t.Fatal("an unreadable catalogue was not an unstarted pass", err)
	}
	f.catalogErr = nil
	listing := errors.New("native unavailable")
	f.hook = func(_ context.Context, verb, _ string) error {
		if verb == "list" {
			return listing
		}
		return nil
	}
	if err := f.c.SyncAll(t.Context()); !cronsync.Unstarted(err) || !errors.Is(err, listing) {
		t.Fatal("an unreadable native list was not an unstarted pass", err)
	}
	f.hook = nil
	f.gitHook = func(context.Context, string, string) error { return errors.New("git unavailable") }
	if err := f.c.SyncAll(t.Context()); err == nil || cronsync.Unstarted(err) {
		t.Fatal("a repository's own failure was taken for an unstarted pass", err)
	}
}
