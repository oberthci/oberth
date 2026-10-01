package gitcache

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultPolicyRequiresFreshUpstreamAndRegularBlob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	if _, err := cache.Ensure(ctx, "example"); err != nil {
		t.Fatal(err)
	}
	if _, body, exists, err := cache.ReadDefaultBlob(ctx, "example", "missing", 4096); err != nil || exists || len(body) != 0 {
		t.Fatalf("absent policy = %q, %v, %v", body, exists, err)
	}
	sha := repository.commitFile(t, "require policy", "policy.yaml", "required\n")
	actual, body, exists, err := cache.ReadDefaultBlob(ctx, "example", "policy.yaml", 4096)
	if err != nil || !exists || actual != sha || string(body) != "required\n" {
		t.Fatalf("fresh policy = %s, %q, %v, %v", actual, body, exists, err)
	}
	if _, _, err := cache.ReadOptionalBlob(ctx, "example", sha, "policy.yaml", 1); err == nil {
		t.Fatal("oversized policy accepted")
	}
	if _, exists, err := cache.ReadOptionalBlob(ctx, "example", strings.Repeat("f", 40), "missing", 4096); err == nil || exists {
		t.Fatal("unreadable commit became absent policy")
	}
	if err := os.Symlink("policy.yaml", filepath.Join(repository.work, "linked")); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository.work, "add", "linked")
	runGit(t, repository.work, "commit", "-m", "symlink")
	runGit(t, repository.work, "push", "origin", "main")
	if _, _, _, err := cache.ReadDefaultBlob(ctx, "example", "linked", 4096); err == nil {
		t.Fatal("symlink accepted as policy")
	}
	if err := os.Rename(repository.upstream, repository.upstream+".offline"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := cache.ReadDefaultBlob(ctx, "example", "policy.yaml", 4096); err == nil {
		t.Fatal("stale policy accepted after failed upstream refresh")
	}
}

func TestDefaultPolicyIgnoresLocallyPushedMain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository := newTestRepository(t)
	sha := repository.commitFile(t, "required", "policy.yaml", "upstream policy\n")
	cache := newTestCache(t, repository.upstream)
	cached, err := cache.Ensure(ctx, "example")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository.work, "policy.yaml"), []byte("candidate policy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository.work, "add", "policy.yaml")
	runGit(t, repository.work, "commit", "-m", "candidate")
	runGit(t, repository.work, "push", cached.Path, "main")
	actual, body, exists, err := cache.ReadDefaultBlob(ctx, "example", "policy.yaml", 4096)
	if err != nil || !exists || actual != sha || string(body) != "upstream policy\n" {
		t.Fatalf("public main replaced required upstream policy: %s %q %v %v", actual, body, exists, err)
	}
}

func TestDefaultPolicyProvesUnbornBaselineWithoutIgnoringCandidate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	upstream := filepath.Join(root, "empty.git")
	runGit(t, "", "init", "--bare", "--initial-branch=main", upstream)
	cache := newTestCache(t, upstream)
	cached, err := cache.Ensure(ctx, "example")
	if err != nil {
		t.Fatal(err)
	}
	sha, body, exists, err := cache.ReadDefaultBlob(ctx, "example", ".oberth/tests.yaml", 4096)
	if err != nil || sha != "" || exists || len(body) != 0 {
		t.Fatalf("unborn baseline: %q %q %v %v", sha, body, exists, err)
	}
	work := filepath.Join(root, "candidate")
	runGit(t, "", "init", "--initial-branch=feature", work)
	runGit(t, work, "config", "user.email", "test@example.invalid")
	runGit(t, work, "config", "user.name", "Test")
	if err := os.Mkdir(filepath.Join(work, ".oberth"), 0o700); err != nil {
		t.Fatal(err)
	}
	contract := "version: 1\nprofile: ebpf-offline-amd64-v1\nartifact: ebpf/secret_monitor.o\n"
	if err := os.WriteFile(filepath.Join(work, ".oberth/tests.yaml"), []byte(contract), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", ".oberth/tests.yaml")
	runGit(t, work, "commit", "-m", "required candidate")
	runGit(t, work, "push", cached.Path, "feature")
	candidateSHA := runGit(t, work, "rev-parse", "HEAD")
	if body, exists, err := cache.ReadOptionalBlob(ctx, "example", candidateSHA, ".oberth/tests.yaml", 4096); err != nil || !exists || string(body) != contract {
		t.Fatalf("candidate requirement lost: %q %v %v", body, exists, err)
	}
	if _, _, exists, err := cache.ReadDefaultBlob(ctx, "example", ".oberth/tests.yaml", 4096); err != nil || exists {
		t.Fatalf("local candidate invented default: %v %v", exists, err)
	}
	// An upstream with objects/refs but no declared main is not empty.
	runGit(t, work, "tag", "only-tag")
	runGit(t, work, "push", upstream, "refs/tags/only-tag")
	if _, _, _, err := cache.ReadDefaultBlob(ctx, "example", ".oberth/tests.yaml", 4096); err == nil {
		t.Fatal("nonempty missing-default upstream treated as unborn")
	}
}
