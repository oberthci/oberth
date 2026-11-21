package gitcache_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func TestGitPortPublication(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	var port gitport.Git = c
	f.Git(t, f.Work, "checkout", "-b", "feature")
	f.Write(t, "feature", []byte("feature"))
	next := f.CommitFile(t, "feature")
	f.Git(t, f.Store(), "fetch", f.Work, "refs/heads/feature:refs/heads/feature")
	ref := gitport.Ref{Name: "refs/heads/feature", ObjectSHA: next, CommitSHA: next}
	request := publicationFor(f, ref.Name, next, "")
	if _, err := c.Publish(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if got, err := port.RemoteRef(t.Context(), gittest.Repository, ref.Name); err != nil || got.ObjectSHA != next {
		t.Fatalf("remote %+v %v", got, err)
	}
	if _, err := c.Publish(t.Context(), request); err != nil {
		t.Fatalf("delivered retry: %v", err)
	}
	refs, err := port.ListRefs(t.Context(), gittest.Repository)
	if err != nil || len(refs) != 2 {
		t.Fatalf("refs %+v %v", refs, err)
	}

}
func TestOlderBranchResultCannotReplaceNewerHead(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	f.Write(t, "new", []byte("head"))
	next := f.CommitFile(t, "new")
	f.Git(t, f.Store(), "fetch", f.Work, "refs/heads/main:refs/heads/main")
	request := publicationFor(f, "refs/heads/main", f.Commit, next)
	// Change upstream so this is not an identical delivered retry.
	f.Git(t, f.Work, "push", f.Bare, "main")
	if _, err := c.Publish(t.Context(), request); err == nil {
		t.Fatal("older result accepted")
	}
}
func TestPromotionRetainsExactResultAndRejectsChangedTarget(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	f.Git(t, f.Work, "checkout", "-b", "feature")
	f.Write(t, "feature", []byte("feature"))
	next := f.CommitFile(t, "feature")
	f.Git(t, f.Store(), "fetch", f.Work, next)
	target := gitport.Ref{Name: "refs/heads/main", ObjectSHA: f.Commit, CommitSHA: f.Commit}
	request := gitport.PromotionRequest{
		Repository:    gittest.Repository,
		Source:        sourcecontract.Revision{Repository: gittest.Repository, Commit: next},
		Target:        target,
		ActionReceipt: "promotion",
	}
	result, err := c.PreparePromotion(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Result.CommitSHA == "" || len(result.Conflicts) > 0 {
		t.Fatalf("bad result %+v", result)
	}
	retry, err := c.PreparePromotion(t.Context(), request)
	if err != nil || result.Result != retry.Result {
		t.Fatalf("unstable result %+v %v", retry, err)
	}
	kept := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/keep/")
	if !strings.Contains(kept, "/promotion-") {
		t.Fatal("unretained promotion")
	}
	if _, err = c.Publish(t.Context(), promotionPublication(f, result)); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Publish(t.Context(), promotionPublication(f, result)); err != nil {
		t.Fatal(err)
	}
	f.Git(t, f.Work, "checkout", "main")
	f.Write(t, "independent", []byte("change"))
	changed := f.CommitFile(t, "upstream")
	f.Git(t, f.Work, "push", "--force", f.Bare, "main")
	if changed == result.Result.CommitSHA {
		t.Fatal("no target change")
	}
	if _, err = c.Publish(t.Context(), promotionPublication(f, result)); err == nil {
		t.Fatal("changed target accepted")
	}
}
func TestPromotionReturnsConflictEvidence(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	f.Write(t, "file.txt", []byte("first\n"))
	first := f.CommitFile(t, "first")
	f.Git(t, f.Work, "checkout", "--detach", f.Commit)
	f.Write(t, "file.txt", []byte("second\n"))
	second := f.CommitFile(t, "second")
	for _, oid := range []string{first, second} {
		f.Git(t, f.Store(), "fetch", f.Work, oid)
	}
	result, err := c.PreparePromotion(
		t.Context(),
		gitport.PromotionRequest{
			Repository:    gittest.Repository,
			Source:        sourcecontract.Revision{Repository: gittest.Repository, Commit: first},
			Target:        gitport.Ref{Name: "refs/heads/main", ObjectSHA: second, CommitSHA: second},
			ActionReceipt: "promotion-conflict",
		},
	)
	if err != nil || len(result.Conflicts) != 1 || result.Conflicts[0] != "file.txt" || result.Result.ObjectSHA != "" {
		t.Fatalf("%+v %v", result, err)
	}
}
func TestFrozenTagsAreNotRefreshedAtLoad(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	b := f.Binding()
	f.Git(t, f.Store(), "tag", "new", f.Commit)
	stage, err := c.Stage(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	files := readArchive(t, stage.Body)
	if _, ok := files[".git/refs/tags/new"]; ok {
		t.Fatal("fresh tag leaked into admitted snapshot")
	}
}
func TestFetchPreservesAcceptedPublicRefOwnership(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	if err := c.ForceFetch(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	f.Write(t, "upstream", []byte("upstream"))
	next := f.CommitFile(t, "next")
	f.Git(t, f.Work, "push", f.Bare, "main")
	if err := c.ForceFetch(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	head, err := c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/main")
	if err != nil || head.ObjectSHA != next {
		t.Fatalf("refresh %+v %v", head, err)
	}
	f.Git(t, f.Store(), "update-ref", "refs/heads/main", f.Commit)
	f.Write(t, "later", []byte("later"))
	f.CommitFile(t, "later")
	f.Git(t, f.Work, "push", f.Bare, "main")
	if err = c.ForceFetch(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	head, err = c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/main")
	if err != nil || head.ObjectSHA != f.Commit {
		t.Fatalf("overwrote local ownership: %+v %v", head, err)
	}
}
func TestTagPublicationKeepsRawIdentityAndNeverForces(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	f.Git(t, f.Work, "tag", "-a", "release", "-m", "release")
	f.Git(t, f.Store(), "fetch", f.Work, "refs/tags/release:refs/tags/release")
	tag, err := c.ResolveRef(t.Context(), gittest.Repository, "refs/tags/release")
	if err != nil {
		t.Fatal(err)
	}
	req := publicationFor(f, tag.Name, tag.ObjectSHA, "")
	req.CommitSHA = &tag.CommitSHA
	if _, err = c.Publish(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	remote, err := c.RemoteRef(t.Context(), gittest.Repository, tag.Name)
	if err != nil || remote.ObjectSHA != tag.ObjectSHA || remote.CommitSHA != f.Commit {
		t.Fatalf("%+v %v", remote, err)
	}
	previous := tag.ObjectSHA
	req.PreviousSHA = &previous
	if _, err = c.Publish(t.Context(), req); err == nil {
		t.Fatal("tag force accepted")
	}
}
func TestRestoreFailureNeverDeletesOrphans(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, func(context.Context, string) (gitcache.OwnerPage, error) {
		return gitcache.OwnerPage{
			Owners: []gitcache.Owner{
				{ID: "missing", Bindings: f.Bindings(strings.Repeat("f", 40))},
			},
		}, nil
	})
	if err := c.Keep(t.Context(), "orphan", f.Bindings(f.Commit)); err != nil {
		t.Fatal(err)
	}
	if err := c.GC(t.Context()); err == nil {
		t.Fatal("missing root allowed cleanup")
	}
	if f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/keep/orphan/") == "" {
		t.Fatal("orphan deleted on failed restore")
	}
}

func TestCheckoutFreezesOwnDefaultAndTags(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	f.Git(t, f.Store(), "tag", "before", f.Commit)
	ref := sourcecontract.Ref{Name: "refs/heads/main", ObjectSHA: f.Commit, CommitSHA: f.Commit}
	export, err := c.Checkout(t.Context(), gittest.Repository, ref)
	if err != nil || export.Ref != ref || export.DefaultBranch != ref || len(export.Tags) != 1 {
		t.Fatalf("checkout %+v %v", export, err)
	}
	f.Git(t, f.Store(), "tag", "after", f.Commit)
	if len(export.Tags) != 1 || export.Tags[0].Name != "refs/tags/before" {
		t.Fatal("snapshot changed", export)
	}
	ref.CommitSHA = strings.Repeat("f", 40)
	if _, err = c.Checkout(t.Context(), gittest.Repository, ref); err == nil {
		t.Fatal("mismatched peeled commit accepted")
	}
}
