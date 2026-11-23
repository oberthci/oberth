package gitcache_test

import (
	"errors"
	"os"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func TestEmptyCheckoutReadsFreshDefaultHeadAndExactFile(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	f.Write(t, "file.txt", []byte("fresh default\n"))
	next := f.CommitFile(t, "advance")
	f.Git(t, f.Work, "push", f.Bare, "main")
	export, err := c.Checkout(t.Context(), gittest.Repository, sourcecontract.Ref{})
	if err != nil || export.Ref.CommitSHA != next || export.Ref != export.DefaultBranch {
		t.Fatalf("default %+v %v", export, err)
	}
	file, err := c.ReadFile(
		t.Context(),
		gitport.Revision{Repository: gittest.Repository, Commit: export.Ref.CommitSHA},
		"file.txt",
	)
	if err != nil || string(file.Bytes) != "fresh default\n" {
		t.Fatalf("file %+v %v", file, err)
	}
	if local := f.Git(t, f.Store(), "rev-parse", "refs/heads/main"); local != f.Commit {
		t.Fatal("head reader moved local branch", local)
	}
}

func TestEmptyCheckoutNeverFallsBackOnAbsentDefaultOrOutage(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	f.Git(t, f.Bare, "update-ref", "-d", "refs/heads/main")
	_, err := c.Checkout(t.Context(), gittest.Repository, sourcecontract.Ref{})
	var lost *gitport.ForgeHistoryLostError
	if !errors.As(err, &lost) {
		t.Fatal("empty forge not explicit", err)
	}
	if err = os.Rename(f.Bare, f.Bare+".offline"); err != nil {
		t.Fatal(err)
	}
	_, err = c.Checkout(t.Context(), gittest.Repository, sourcecontract.Ref{})
	var unavailable *gitport.UnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatal("outage not explicit", err)
	}
}
