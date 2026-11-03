package gitcache

import (
	"strings"
	"testing"
)

func TestReceiveRefusesDefaultBranchDeletion(t *testing.T) {
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, ready.Path, "config", "receive.denyDeleteCurrent", "ignore")
	output, err := pushWithReceivePack(repository.work, ready.Path, backendServiceCommand(t, cache, ReceivePack), ":refs/heads/main")
	if err == nil || !strings.Contains(output, "default branch") {
		t.Fatalf("deletion accepted: %v\n%s", err, output)
	}
	if got := remoteRef(t, ready.Path, "refs/heads/main"); got != repository.initialSHA {
		t.Fatalf("local main = %s", got)
	}
	if got := remoteRef(t, repository.upstream, "refs/heads/main"); got != repository.initialSHA {
		t.Fatalf("upstream main = %s", got)
	}
}

func TestDeleteBranchRefusesDefaultBranch(t *testing.T) {
	repository := newTestRepository(t)
	runGit(t, repository.upstream, "config", "receive.denyDeleteCurrent", "ignore")
	cache := newTestCache(t, repository.upstream)
	if _, err := cache.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	if err := cache.DeleteBranch(t.Context(), "example", "main"); err == nil || !strings.Contains(err.Error(), "default branch") {
		t.Fatalf("deletion = %v", err)
	}
	if got := remoteRef(t, repository.upstream, "refs/heads/main"); got != repository.initialSHA {
		t.Fatalf("upstream main = %s", got)
	}
}
