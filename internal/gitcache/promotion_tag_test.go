package gitcache

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// promote tag_after (#1524): an annotated tag on the promoted commit, pushed
// without force; a retry reuses the tag object and a foreign tag is refused.
func TestPublishAnnotatedTag(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	object, err := cache.PublishAnnotatedTag(context.Background(), "example", "deploy/1", repository.initialSHA, "apply the promoted result")
	if err != nil {
		t.Fatal(err)
	}
	if got := remoteRef(t, repository.upstream, "refs/tags/deploy/1"); got != object || object == repository.initialSHA {
		t.Fatalf("upstream tag = %s, want annotated object %s", got, object)
	}
	if kind := strings.TrimSpace(runGit(t, ready.Path, "cat-file", "-t", object)); kind != "tag" {
		t.Fatalf("tag object type = %s", kind)
	}
	if body := runGit(t, ready.Path, "cat-file", "-p", object); !strings.Contains(body, "object "+repository.initialSHA) || !strings.Contains(body, "apply the promoted result") || !strings.Contains(body, "tagger Oberth <oberth@localhost>") {
		t.Fatalf("tag object = %s", body)
	}
	again, err := cache.PublishAnnotatedTag(context.Background(), "example", "deploy/1", repository.initialSHA, "apply the promoted result")
	if err != nil || again != object {
		t.Fatalf("retry = %s, %v; want the same object %s", again, err, object)
	}
	feature := repository.featureCommit(t, "feature/tagged", "tagged\n")
	runGit(t, repository.work, "push", ready.Path, feature+":refs/heads/feature/tagged")
	if _, err := cache.PublishAnnotatedTag(context.Background(), "example", "deploy/1", feature, "elsewhere"); !errors.Is(err, ErrTagExists) {
		t.Fatalf("tag at another commit = %v", err)
	}
	// A tag the upstream already has elsewhere is refused and leaves no tag
	// on the mirror.
	runGit(t, repository.work, "tag", "-a", "-m", "foreign", "deploy/3", feature)
	runGit(t, repository.work, "push", repository.upstream, "refs/tags/deploy/3")
	if _, err := cache.PublishAnnotatedTag(context.Background(), "example", "deploy/3", repository.initialSHA, "taken upstream"); !errors.Is(err, ErrTagExists) {
		t.Fatalf("tag taken upstream = %v", err)
	}
	if out := runGit(t, ready.Path, "for-each-ref", "refs/tags/deploy/3"); out != "" {
		t.Fatalf("refused tag stayed on the mirror: %s", out)
	}
	// Another writer's annotated tag on the same commit is never pushed.
	// Seed the same fresh admission anchor a normal receive prepares.
	lock := cache.repoLock(ready.Path)
	lock.Lock()
	_, err = cache.prepareReleaseAdmissionLocked(context.Background(), ready.Path, true)
	lock.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, repository.work, "tag", "-a", "-m", "release", "deploy/4", repository.initialSHA)
	runGit(t, repository.work, "push", ready.Path, "refs/tags/deploy/4")
	if _, err := cache.PublishAnnotatedTag(context.Background(), "example", "deploy/4", repository.initialSHA, "not ours"); !errors.Is(err, ErrTagExists) {
		t.Fatalf("foreign mirror tag = %v", err)
	}
	if got := runGit(t, "", "ls-remote", repository.upstream, "refs/tags/deploy/4"); got != "" {
		t.Fatalf("foreign mirror tag was pushed upstream: %s", got)
	}
	for _, bad := range []string{"", "bad..name", "-x"} {
		if _, err := cache.PublishAnnotatedTag(context.Background(), "example", bad, repository.initialSHA, "m"); err == nil {
			t.Fatalf("tag %q accepted", bad)
		}
	}
	if _, err := cache.PublishAnnotatedTag(context.Background(), "example", "deploy/2", repository.initialSHA, " \n"); err == nil {
		t.Fatal("empty message accepted")
	}
}

func TestUpstreamContainsPromotedResultAfterTargetMoves(t *testing.T) {
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	if _, err := cache.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	next := repository.featureCommit(t, "feature/later", "later\n")
	runGit(t, repository.work, "push", repository.upstream, next+":refs/heads/main")
	if ok, err := cache.UpstreamContainsCommit(t.Context(), "example", "main", repository.initialSHA); err != nil || !ok {
		t.Fatalf("advanced target: %v %v", ok, err)
	}
	runGit(t, repository.work, "push", "--force", repository.upstream, repository.initialSHA+":refs/heads/main")
	if ok, err := cache.UpstreamContainsCommit(t.Context(), "example", "main", next); err != nil || ok {
		t.Fatalf("rewound target: %v %v", ok, err)
	}
}
