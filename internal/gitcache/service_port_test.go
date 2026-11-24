package gitcache_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
)

func publicationFor(f *gittest.Fixture, ref, object, previous string) model.Publication {
	r := model.Publication{ID: "publish",
		Repo:        gittest.Repository,
		Ref:         ref,
		ObjectSHA:   object,
		PreviousSHA: &previous,
		ReceivedSHA: new(previous),
		Actor:       "actor"}
	if object != "" {
		r.CommitSHA = &object
	}
	return r
}

func TestPublishExactCreateRetryAndRunlessDeletion(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	ref := "refs/heads/topic"
	f.Git(t, f.Store(), "update-ref", ref, f.Commit)
	r := publicationFor(f, ref, f.Commit, "")
	for attempt := 1; attempt <= 2; attempt++ {
		r.Attempts = attempt
		if receipt, err := c.Publish(t.Context(), r); err != nil || receipt.ActionReceipt != r.ID {
			t.Fatal(receipt, err)
		}
	}
	f.Git(t, f.Store(), "update-ref", "-d", ref)
	r = publicationFor(f, ref, "", f.Commit)
	for attempt := 1; attempt <= 2; attempt++ {
		r.Attempts = attempt
		if _, err := c.Publish(t.Context(), r); err != nil {
			t.Fatal("deletion/retry", err)
		}
	}
	if got := f.Git(t, f.Bare, "for-each-ref", "--format=%(refname)", ref); got != "" {
		t.Fatal("deletion resurrected", got)
	}
}

func TestPublishDistinguishesLocalUpstreamUnknownAndOutage(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	f.Write(t, "next", []byte("next"))
	next := f.CommitFile(t, "next")
	f.Git(t, f.Store(), "fetch", f.Work, next)
	ref := "refs/heads/topic"
	f.Git(t, f.Store(), "update-ref", ref, next)
	_, err := c.Publish(t.Context(), publicationFor(f, ref, f.Commit, ""))
	checkMoved(t, err, "local")
	tree := f.Git(t, f.Work, "rev-parse", "HEAD^{tree}")
	independent := f.Git(t, f.Work, "commit-tree", tree, "-m", "independent forge head")
	f.Git(t, f.Bare, "fetch", f.Work, independent)
	f.Git(t, f.Bare, "update-ref", ref, independent)
	_, err = c.Publish(t.Context(), publicationFor(f, ref, next, ""))
	checkMoved(t, err, "upstream")
	f.Git(t, f.Store(), "update-ref", "-d", ref)
	_, err = c.Publish(t.Context(), publicationFor(f, ref, next, f.Commit))
	checkMoved(t, err, "local")
	r := publicationFor(f, ref, next, f.Commit)
	r.PreviousSHA = nil
	if _, err = c.Publish(t.Context(), r); err == nil {
		t.Fatal("unknown predecessor accepted")
	}
	// Isolate the forge outage from the preceding local-head refusal.
	f.Git(t, f.Store(), "update-ref", ref, next)
	if err = os.Rename(f.Bare, f.Bare+".offline"); err != nil {
		t.Fatal(err)
	}
	_, err = c.Publish(t.Context(), publicationFor(f, ref, next, f.Commit))
	var unavailable *gitport.UnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatal("outage misclassified", err)
	}
}
func checkMoved(t *testing.T, err error, location string) {
	t.Helper()
	var moved *gitport.RefMovedError
	var unavailable *gitport.UnavailableError
	if !errors.As(err, &moved) || moved.Location != location || errors.As(err, &unavailable) {
		t.Fatal("wrong movement class", location, err)
	}
}

func TestDefaultFileFreshHeadAbsentAndOutage(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	f.Write(t, "file.txt", []byte("fresh policy bytes"))
	next := f.CommitFile(t, "policy")
	f.Git(t, f.Work, "push", f.Bare, "main")
	head, file, err := c.DefaultFile(t.Context(), gittest.Repository, "file.txt")
	if err != nil || head.CommitSHA != next || string(file.Bytes) != "fresh policy bytes" {
		t.Fatal(head, file, err)
	}
	head, _, err = c.DefaultFile(t.Context(), gittest.Repository, "missing-policy")
	var absent *gitport.AbsentError
	if !errors.As(err, &absent) || head.CommitSHA != next {
		t.Fatal("missing file lost known head", head, err)
	}
	if err = os.Rename(f.Bare, f.Bare+".offline"); err != nil {
		t.Fatal(err)
	}
	_, _, err = c.DefaultFile(t.Context(), gittest.Repository, "file.txt")
	var unavailable *gitport.UnavailableError
	if !errors.As(err, &unavailable) || errors.As(err, &absent) {
		t.Fatal("outage fell back to local policy", err)
	}
}

func TestPreparedPromotionExpiresAt24HoursAndLogsOnce(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	now := time.Unix(2000000000, 0)
	cfg.Now = func() time.Time { return now }
	logs := 0
	cfg.Log = func(reason, id string) {
		checkPromotionExpiryLog(t, reason, id)
		logs++
	}
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	r := gitport.PromotionRequest{Repository: gittest.Repository,
		Source: gitport.Revision{Repository: gittest.Repository,
			Commit: f.Commit},
		Target: gitport.Ref{Name: "refs/heads/main",
			ObjectSHA: f.Commit,
			CommitSHA: f.Commit},
		ActionReceipt: "expiring"}
	if _, err := c.PreparePromotion(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	now = now.Add(gitcache.PromotionLifetime - time.Second)
	if err := c.GC(t.Context()); err != nil {
		t.Fatal(err)
	}
	if refs := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/keep/"); refs == "" || logs != 0 {
		t.Fatal("expired before 24h", refs, logs)
	}
	if _, err := c.PreparePromotion(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	for range 2 {
		if err := c.GC(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if refs := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/keep/"); refs != "" || logs != 1 {
		t.Fatal("expiry not released once", refs, logs)
	}
}

func TestPromotionAndTagConflictsAreTyped(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, func(context.Context, string) (gitcache.OwnerPage, error) { return gitcache.OwnerPage{}, nil })
	f.Write(t, "feature", []byte("feature"))
	next := f.CommitFile(t, "feature")
	f.Git(t, f.Store(), "fetch", f.Work, next)
	prepared,
		err := c.PreparePromotion(t.Context(),
		gitport.PromotionRequest{Repository: gittest.Repository,
			Source: gitport.Revision{Repository: gittest.Repository,
				Commit: next},
			Target: gitport.Ref{Name: "refs/heads/main",
				ObjectSHA: f.Commit,
				CommitSHA: f.Commit},
			ActionReceipt: "moved"})
	if err != nil {
		t.Fatal(err)
	}
	// Move to a distinct target so the prepared result is no longer current.
	f.Write(t, "other", []byte("other"))
	other := f.CommitFile(t, "other")
	f.Git(t, f.Bare, "fetch", f.Work, other)
	f.Git(t, f.Bare, "update-ref", "refs/heads/main", other)
	_, err = c.Publish(t.Context(), promotionPublication(f, prepared))
	checkMoved(t, err, "upstream")
	f.Git(t, f.Store(), "update-ref", "refs/tags/release", next)
	f.Git(t, f.Bare, "update-ref", "refs/tags/release", f.Commit)
	_, err = c.Publish(t.Context(), publicationFor(f, "refs/tags/release", next, ""))
	var exists *gitport.AlreadyExistsError
	if !errors.As(err, &exists) {
		t.Fatal("tag conflict not typed", err)
	}

}

func TestPublishReadsBackSuccessAfterLostAcknowledgement(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	wrapper := filepath.Join(f.Root, "git-lost-ack")
	body := "#!/usr/bin/python3\n" +
		"import subprocess,sys\n" +
		"r=subprocess.run(['/usr/bin/git']+sys.argv[1:])\n" +
		"sys.exit(1 if 'push' in sys.argv and r.returncode==0 else r.returncode)\n" +
		""
	if err := os.WriteFile(wrapper, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Binary = wrapper
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	f.Git(t, f.Store(), "update-ref", "refs/heads/topic", f.Commit)
	if _, err := c.Publish(t.Context(), publicationFor(f, "refs/heads/topic", f.Commit, "")); err != nil {
		t.Fatal("readback lost confirmed result", err)
	}
}

func TestSourceRefusalGuidanceHasPunctuationWithoutInternalLabels(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	_, err := c.ReadFile(t.Context(), gitport.Revision{Repository: gittest.Repository, Commit: f.Commit}, "../escape")
	var refused *gitcache.RefusalError
	if !errors.As(err,
		&refused) ||
		!strings.Contains(err.Error(),
			refused.Reason()+". "+refused.Remedy()) ||
		strings.Contains(err.Error(),
			"Path:") {
		t.Fatal(err)
	}
}

func checkPromotionExpiryLog(t *testing.T, reason, id string) {
	t.Helper()
	if reason != "prepared promotion expired" || id == "" {
		t.Error(reason, id)
	}
}

func promotionPublication(f *gittest.Fixture, p gitport.Promotion) model.Publication {
	r := publicationFor(f, p.Result.Name, p.Result.ObjectSHA, p.Request.Target.ObjectSHA)
	r.ID = p.Request.ActionReceipt
	r.PromotionID = p.Request.ActionReceipt
	r.CommitSHA = &p.Result.CommitSHA
	return r
}
