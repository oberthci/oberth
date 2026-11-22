package gitcache_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

// A branch cut from main while main has not moved is a fast-forward: the
// tested commit is the result and publication moves main to exactly it.
func TestPromotionOfABranchFromAnUnmovedMainFastForwards(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	f.Git(t, f.Work, "checkout", "-b", "feature")
	f.Write(t, "feature.txt", []byte("feature\n"))
	f.CommitFileAt(t, "first", targetDate)
	f.Write(t, "feature.txt", []byte("feature, reviewed\n"))
	tested := f.CommitFileAt(t, "tested work", testedDate)
	f.Git(t, f.Store(), "fetch", f.Work, tested)
	objects := f.Git(t, f.Store(), "count-objects", "-v")
	prepared, err := c.PreparePromotion(t.Context(), gitport.PromotionRequest{
		Repository:    gittest.Repository,
		Source:        sourcecontract.Revision{Repository: gittest.Repository, Commit: tested},
		Target:        gitport.Ref{Name: "refs/heads/main", ObjectSHA: f.Commit, CommitSHA: f.Commit},
		ActionReceipt: "promotion-forward",
	})
	if err != nil || len(prepared.Conflicts) > 0 || prepared.Result.CommitSHA != tested || prepared.Result.ObjectSHA != tested {
		t.Fatalf("%+v %v", prepared, err)
	}
	if want := f.Git(t, f.Store(), "rev-parse", tested+"^{tree}"); prepared.Tree != want {
		t.Fatalf("tree %s, tested tree %s", prepared.Tree, want)
	}
	if after := f.Git(t, f.Store(), "count-objects", "-v"); after != objects {
		t.Fatalf("fast-forward wrote objects:\n%s\n%s", objects, after)
	}
	if _, err = c.Publish(t.Context(), promotionPublication(f, prepared)); err != nil {
		t.Fatal(err)
	}
	if got := f.Git(t, f.Bare, "rev-parse", "refs/heads/main"); got != tested {
		t.Fatalf("main at %s, tested %s", got, tested)
	}
}

// A tested descendant committed no later than main is no fast-forward: its own
// date would not rank above main, so the promotion writes the merge dated one
// second after main.
func TestPromotionOfADescendantNotNewerThanMainMerges(t *testing.T) {
	t.Parallel()
	for name, tested := range map[string]string{"same second": laterDate, "older": testedDate} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := gittest.New(t)
			c := f.Cache(t, nil)
			f.Write(t, "main.txt", []byte("main\n"))
			base := f.CommitFileAt(t, "main moves", laterDate)
			f.Write(t, "feature.txt", []byte("feature\n"))
			source := f.CommitFileAt(t, "tested work", tested)
			f.Git(t, f.Store(), "fetch", f.Work, base, source)
			prepared, err := c.PreparePromotion(t.Context(), gitport.PromotionRequest{
				Repository:    gittest.Repository,
				Source:        sourcecontract.Revision{Repository: gittest.Repository, Commit: source},
				Target:        gitport.Ref{Name: "refs/heads/main", ObjectSHA: base, CommitSHA: base},
				ActionReceipt: "promotion-not-newer",
			})
			if err != nil || prepared.Result.CommitSHA == source {
				t.Fatalf("%+v %v", prepared.Result, err)
			}
			if got, want := commitSeconds(t, f, "%ct", prepared.Result.CommitSHA), commitSeconds(t, f, "%ct", base)+1; got != want {
				t.Fatalf("merge dated %d, want %d", got, want)
			}
		})
	}
}

// A main that moved after the branch was cut is no fast-forward: the
// promotion writes the dated merge of the two.
func TestPromotionOfABranchBesideAMovedMainMerges(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	request := divergedPromotion(t, f, targetDate, testedDate)
	prepared, err := c.PreparePromotion(t.Context(), request)
	if err != nil || len(prepared.Conflicts) > 0 {
		t.Fatalf("%+v %v", prepared, err)
	}
	parents := f.Git(t, f.Store(), "show", "--no-patch", "--format=%P", prepared.Result.CommitSHA)
	if parents != request.Target.CommitSHA+" "+request.Source.Commit {
		t.Fatalf("merge parents %q", parents)
	}
}
