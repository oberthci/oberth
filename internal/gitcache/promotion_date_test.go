package gitcache_test

import (
	"strconv"
	"testing"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

const (
	targetDate = "2026-11-19T08:00:00Z"
	testedDate = "2026-11-20T09:30:00Z"
	laterDate  = "2026-11-21T10:45:00Z"
)

// divergedPromotion commits a target on main and a tested source beside it,
// each at its own date, so preparing the promotion writes a merge commit.
func divergedPromotion(t *testing.T, f *gittest.Fixture, target, tested string) gitport.PromotionRequest {
	t.Helper()
	f.Write(t, "main.txt", []byte("main\n"))
	base := f.CommitFileAt(t, "main moves", target)
	f.Git(t, f.Work, "checkout", "-b", "feature", f.Commit)
	f.Write(t, "feature.txt", []byte("feature\n"))
	source := f.CommitFileAt(t, "tested work", tested)
	f.Git(t, f.Store(), "fetch", f.Work, base, source)
	return gitport.PromotionRequest{
		Repository:    gittest.Repository,
		Source:        sourcecontract.Revision{Repository: gittest.Repository, Commit: source},
		Target:        gitport.Ref{Name: "refs/heads/main", ObjectSHA: base, CommitSHA: base},
		ActionReceipt: "promotion-date",
	}
}

func commitSeconds(t *testing.T, f *gittest.Fixture, format, oid string) int64 {
	t.Helper()
	seconds, err := strconv.ParseInt(f.Git(t, f.Store(), "show", "--no-patch", "--format="+format, oid), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return seconds
}

func pseudoVersion(t *testing.T, f *gittest.Fixture, oid string) string {
	t.Helper()
	return module.PseudoVersion("v0", "", time.Unix(commitSeconds(t, f, "%ct", oid), 0), oid[:12])
}

func TestPromotionCommitCarriesTheTestedCommitDate(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	request := divergedPromotion(t, f, targetDate, testedDate)
	prepared, err := c.PreparePromotion(t.Context(), request)
	if err != nil || len(prepared.Conflicts) > 0 {
		t.Fatalf("%+v %v", prepared, err)
	}
	result := prepared.Result.CommitSHA
	if result == request.Source.Commit || result == request.Target.CommitSHA {
		t.Fatal("promotion wrote no merge commit", result)
	}
	tested := commitSeconds(t, f, "%ct", request.Source.Commit)
	if author, committer := commitSeconds(t, f, "%at", result), commitSeconds(t, f, "%ct", result); author != tested || committer != tested {
		t.Fatalf("promotion dated %d/%d, tested commit %d", author, committer, tested)
	}
	want := module.PseudoVersion("v0", "", time.Unix(tested, 0), result[:12])
	if got := pseudoVersion(t, f, result); got != want || got[len("v0.0.0-"):][:14] != "20261120093000" {
		t.Fatalf("pseudo-version %s, want %s", got, want)
	}
	if base := pseudoVersion(t, f, request.Target.CommitSHA); semver.Compare(pseudoVersion(t, f, result), base) <= 0 {
		t.Fatalf("promotion ranks at or below the target %s", base)
	}
}

func TestPromotionCommitIsTheSameOnEveryPreparation(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	request := divergedPromotion(t, f, targetDate, testedDate)
	first, err := c.PreparePromotion(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.ActionReceipt = "promotion-date-again"
	second, err := c.PreparePromotion(t.Context(), request)
	if err != nil || second.Result != first.Result || second.Tree != first.Tree {
		t.Fatalf("second preparation %+v, first %+v: %v", second.Result, first.Result, err)
	}
}

// A target that is not older than the tested commit dates the promotion one
// second after itself, so the promotion ranks above the head it replaces.
func TestPromotionCommitIsNewerThanItsTarget(t *testing.T) {
	t.Parallel()
	for name, target := range map[string]string{"newer target": laterDate, "same date": testedDate} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			checkPromotionAfterTarget(t, target)
		})
	}
}
func checkPromotionAfterTarget(t *testing.T, targetAt string) {
	t.Helper()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	request := divergedPromotion(t, f, targetAt, testedDate)
	prepared, err := c.PreparePromotion(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	result := prepared.Result.CommitSHA
	target := commitSeconds(t, f, "%ct", request.Target.CommitSHA)
	if author, committer := commitSeconds(t, f, "%at", result), commitSeconds(t, f, "%ct", result); author != target+1 || committer != target+1 {
		t.Fatalf("promotion dated %d/%d, target %d", author, committer, target)
	}
	if semver.Compare(pseudoVersion(t, f, result), pseudoVersion(t, f, request.Target.CommitSHA)) <= 0 {
		t.Fatal("promotion ranks at or below the target")
	}
}

// A promotion onto an empty target writes no commit: the tested commit is the result.
func TestPromotionOntoAnEmptyTargetKeepsTheTestedCommit(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	f.Write(t, "feature.txt", []byte("feature\n"))
	source := f.CommitFileAt(t, "tested work", testedDate)
	f.Git(t, f.Store(), "fetch", f.Work, source)
	prepared, err := c.PreparePromotion(t.Context(), gitport.PromotionRequest{
		Repository:    gittest.Repository,
		Source:        sourcecontract.Revision{Repository: gittest.Repository, Commit: source},
		Target:        gitport.Ref{Name: "refs/heads/main"},
		ActionReceipt: "promotion-empty",
	})
	if err != nil || prepared.Result.CommitSHA != source || prepared.Result.ObjectSHA != source {
		t.Fatalf("%+v %v", prepared.Result, err)
	}
}
