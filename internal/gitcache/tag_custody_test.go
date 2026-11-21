package gitcache_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func TestKeepRetainsAnnotatedOwnTagAfterDeletionAndRestore(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	f.Git(t, f.Work, "tag", "-a", "release", "-m", "exact release annotation", f.Commit)
	tag := f.Git(t, f.Work, "rev-parse", "refs/tags/release")
	f.Git(t, f.Work, "push", f.Bare, "refs/tags/release")
	b := f.Binding()
	b.Export.Ref = sourcecontract.Ref{Name: "refs/tags/release", ObjectSHA: tag, CommitSHA: f.Commit}
	bindings := []sourcecontract.Binding{b}
	c := f.Cache(t, func(context.Context, string) (gitcache.OwnerPage, error) {
		return gitcache.OwnerPage{Owners: []gitcache.Owner{{ID: "release-run", Bindings: bindings}}}, nil
	})
	if err := c.Keep(t.Context(), "release-run", bindings); err != nil {
		t.Fatal(err)
	}
	keep := "refs/oberth/keep/release-run/" + tag
	if got := f.Git(t, f.Store(), "rev-parse", keep); got != tag {
		t.Fatal("raw tag not pinned", got)
	}
	for _, ref := range []string{"refs/tags/release", "refs/oberth/upstream/tags/release", keep} {
		f.Git(t, f.Store(), "update-ref", "-d", ref)
	}
	if err := c.GC(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.Git(t, f.Store(), "rev-parse", keep); got != tag {
		t.Fatal("restore omitted raw tag", got)
	}
	if body := f.Git(t, f.Store(), "cat-file", "tag", tag); !strings.Contains(body, "exact release annotation") {
		t.Fatal(body)
	}
	stage, err := c.Stage(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	files := readArchive(t, stage.Body)
	if string(files[".git/refs/tags/release"]) != tag+"\n" {
		t.Fatal("checkout lost raw tag", files[".git/refs/tags/release"])
	}
}
