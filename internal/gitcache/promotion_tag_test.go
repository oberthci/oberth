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
	for _, bad := range []string{"", "bad..name", "-x"} {
		if _, err := cache.PublishAnnotatedTag(context.Background(), "example", bad, repository.initialSHA, "m"); err == nil {
			t.Fatalf("tag %q accepted", bad)
		}
	}
	if _, err := cache.PublishAnnotatedTag(context.Background(), "example", "deploy/2", repository.initialSHA, " \n"); err == nil {
		t.Fatal("empty message accepted")
	}
}
