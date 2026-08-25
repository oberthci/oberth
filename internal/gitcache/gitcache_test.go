package gitcache

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseRepoPathAcceptsOrgQualifiedAndBare(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input    string
		wantOrg  string
		wantRepo string
	}{
		{"example", "", "example"},
		{"/example.git", "", "example"},
		{"acme/example", "acme", "example"},
		{"/acme/example.git", "acme", "example"},
		{"oberthci/oberth", "oberthci", "oberth"},
		{"cloud-taser/my_repo.git", "cloud-taser", "my_repo"},
	}
	for _, test := range tests {
		org, repo, err := ParseRepoPath(test.input)
		if err != nil {
			t.Fatalf("ParseRepoPath(%q): %v", test.input, err)
		}
		if org != test.wantOrg || repo != test.wantRepo {
			t.Fatalf("ParseRepoPath(%q) = (%q, %q), want (%q, %q)", test.input, org, repo, test.wantOrg, test.wantRepo)
		}
	}
	for _, value := range []string{
		"../secret",
		"a/b/c",
		"owner/../../secret",
		"org/repo\nother",
		"-bad/repo",
		"org/-bad",
		".git",
		"",
	} {
		if _, _, err := ParseRepoPath(value); err == nil {
			t.Fatalf("ParseRepoPath(%q) unexpectedly succeeded", value)
		}
	}
}

func TestValidationAndProtocolAllowlist(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"../secret", "owner/repo", "repo/../../secret", "repo\nother", ".git"} {
		if _, err := NormalizeRepo(value); err == nil {
			t.Fatalf("NormalizeRepo(%q) unexpectedly succeeded", value)
		}
	}
	overlongRefPart := strings.Repeat("a", maximumRefPartBytes+1)
	for _, value := range []string{"../main", "main..next", "refs/heads/main", "refs/main", "-bad", "bad.lock", "bad name", overlongRefPart} {
		if err := ValidateBranch(value); err == nil {
			t.Fatalf("ValidateBranch(%q) unexpectedly succeeded", value)
		}
		if err := ValidateTag(value); err == nil {
			t.Fatalf("ValidateTag(%q) unexpectedly succeeded", value)
		}
	}
	if repo, err := NormalizeRepo("/acme-cli.git"); err != nil || repo != "acme-cli" {
		t.Fatalf("NormalizeRepo canonical path = %q, %v", repo, err)
	}
	for _, service := range []string{"git-upload-pack", "git-receive-pack"} {
		if _, err := ParseService(service); err != nil {
			t.Fatalf("ParseService(%q): %v", service, err)
		}
	}
	if _, err := ParseService("git-shell"); err == nil {
		t.Fatal("non-Git service unexpectedly allowed")
	}
	for _, value := range []string{
		"ssh://git@codeberg.org/acme/example.git",
		"https://codeberg.org/acme/example.git",
		filepath.Join(t.TempDir(), "upstream.git"),
	} {
		if err := ValidateUpstream(value); err != nil {
			t.Fatalf("ValidateUpstream(%q): %v", value, err)
		}
	}
	for _, value := range []string{
		"ext::sh -c id",
		"file:///tmp/repo.git",
		"git://codeberg.org/acme/example.git",
		"http://codeberg.org/acme/example.git",
		"https://token@codeberg.org/acme/example.git",
		"ssh://git:password@codeberg.org/acme/example.git",
		"--upload-pack=helper",
		"ssh://git@codeberg.org/acme/example.git\n--config=x",
		"ssh://git@codeberg.org/acme/example%0a.git",
		"ssh://-oProxyCommand@codeberg.org/acme/example.git",
		"ssh://git@codeberg.org/acme/%2e%2e/example.git",
	} {
		if err := ValidateUpstream(value); err == nil {
			t.Fatalf("ValidateUpstream(%q) unexpectedly succeeded", value)
		}
	}
}

func TestValidateUpstreamSchemeErrorContainsGuidance(t *testing.T) {
	t.Parallel()
	for _, scheme := range []string{"git", "http", "file", "ftp"} {
		remote := scheme + "://host/repo"
		err := ValidateUpstream(remote)
		if err == nil {
			t.Fatalf("ValidateUpstream(%q) should fail", remote)
		}
		if !strings.Contains(err.Error(), "use ssh:// or https://") {
			t.Fatalf("ValidateUpstream(%q) error should mention 'use ssh:// or https://', got: %v", remote, err)
		}
	}
}

func TestEnsureFallsBackToStaleCache(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	first, err := cache.Ensure(context.Background(), "example")
	if err != nil || first.Stale {
		t.Fatalf("initial Ensure = %+v, %v", first, err)
	}
	offline := repository.upstream + ".offline"
	if err := os.Rename(repository.upstream, offline); err != nil {
		t.Fatal(err)
	}
	second, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatalf("stale Ensure: %v", err)
	}
	if !second.Stale || second.Path != first.Path {
		t.Fatalf("stale Ensure = %+v, want path %q and stale", second, first.Path)
	}
	clone := filepath.Join(t.TempDir(), "clone")
	runGit(t, "", "clone", "--quiet", second.Path, clone)
	if got := runGit(t, clone, "rev-parse", "HEAD"); got != repository.initialSHA {
		t.Fatalf("stale cache HEAD = %s, want %s", got, repository.initialSHA)
	}
}

func TestRefSHAResolvesLocalBranchWithoutUpstream(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ctx := context.Background()

	if _, err := cache.Ensure(ctx, "example"); err != nil {
		t.Fatal(err)
	}

	// Resolve the default branch.
	sha, err := cache.RefSHA(ctx, "example", "main")
	if err != nil {
		t.Fatalf("RefSHA(main): %v", err)
	}
	if sha != repository.initialSHA {
		t.Fatalf("RefSHA(main) = %s, want %s", sha, repository.initialSHA)
	}

	// Non-existent branch returns an error.
	if _, err := cache.RefSHA(ctx, "example", "does-not-exist"); err == nil {
		t.Fatal("RefSHA for missing branch unexpectedly succeeded")
	}

	// Works offline (no upstream contact).
	offline := repository.upstream + ".offline"
	if err := os.Rename(repository.upstream, offline); err != nil {
		t.Fatal(err)
	}
	sha, err = cache.RefSHA(ctx, "example", "main")
	if err != nil {
		t.Fatalf("RefSHA offline: %v", err)
	}
	if sha != repository.initialSHA {
		t.Fatalf("RefSHA offline = %s, want %s", sha, repository.initialSHA)
	}
}

func TestEnsureDiscoversAndAllowsNonMainDefaultBranch(t *testing.T) {
	t.Parallel()
	repository := newTestRepositoryWithBranch(t, "trunk")
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	if ready.DefaultBranch != "trunk" {
		t.Fatalf("default branch = %q, want trunk", ready.DefaultBranch)
	}
	if got := runGit(t, ready.Path, "symbolic-ref", "HEAD"); got != "refs/heads/trunk" {
		t.Fatalf("cache HEAD = %q", got)
	}
	featureSHA := repository.featureCommit(t, "feature/non-main", "feature\n")
	receiveArgs := cache.serviceArgs(ReceivePack, ready.Path)
	receivePack := strings.Join(append([]string{cache.gitBinary}, receiveArgs[:len(receiveArgs)-1]...), " ")
	if output, err := pushWithReceivePack(repository.work, ready.Path, receivePack, featureSHA+":refs/heads/trunk"); err != nil {
		t.Fatalf("push trunk: %v\n%s", err, output)
	}
	if got := runGit(t, ready.Path, "rev-parse", "refs/heads/trunk"); got != featureSHA {
		t.Fatalf("trunk = %s, want %s", got, featureSHA)
	}
}

func TestCheckoutUsesExactDetachedSHA(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	if _, err := cache.Ensure(context.Background(), "example"); err != nil {
		t.Fatal(err)
	}
	second := repository.commit(t, "second", "second\n")
	if _, err := cache.Ensure(context.Background(), "example"); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(t.TempDir(), "run", "src")
	if err := cache.Checkout(context.Background(), "example", repository.initialSHA, workspace); err != nil {
		t.Fatal(err)
	}
	if got := runGit(t, workspace, "rev-parse", "HEAD"); got != repository.initialSHA {
		t.Fatalf("workspace HEAD = %s, want %s", got, repository.initialSHA)
	}
	if got := runGit(t, workspace, "symbolic-ref", "-q", "HEAD"); got != "" {
		t.Fatalf("workspace is not detached: %s", got)
	}
	if second == repository.initialSHA {
		t.Fatal("test setup did not create a second commit")
	}
}

func TestSyncBranchForcePublishesAndVerifiesExactSHA(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	branchSHA := repository.featureCommit(t, "feature/one", "feature-one\n")
	runGit(t, repository.work, "push", "--force", ready.Path, branchSHA+":refs/heads/feature/one")
	if err := cache.SyncBranch(context.Background(), "example", "feature/one", branchSHA); err != nil {
		t.Fatal(err)
	}
	if got := remoteRef(t, repository.upstream, "refs/heads/feature/one"); got != branchSHA {
		t.Fatalf("upstream feature = %s, want %s", got, branchSHA)
	}

	replacement := repository.featureCommit(t, "feature/two", "replacement\n")
	runGit(t, repository.work, "push", "--force", ready.Path, replacement+":refs/heads/feature/one")
	if err := cache.SyncBranch(context.Background(), "example", "feature/one", replacement); err != nil {
		t.Fatal(err)
	}
	if got := remoteRef(t, repository.upstream, "refs/heads/feature/one"); got != replacement {
		t.Fatalf("force-synced feature = %s, want %s", got, replacement)
	}
}

func TestDeleteBranchRemovesUpstreamAndLocalRefs(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	branchSHA := repository.featureCommit(t, "feature/delete-me", "ephemeral\n")
	runGit(t, repository.work, "push", "--force", ready.Path, branchSHA+":refs/heads/feature/delete-me")
	if err := cache.SyncBranch(context.Background(), "example", "feature/delete-me", branchSHA); err != nil {
		t.Fatal(err)
	}
	if got := remoteRef(t, repository.upstream, "refs/heads/feature/delete-me"); got != branchSHA {
		t.Fatalf("upstream feature = %s, want %s", got, branchSHA)
	}

	// Delete the branch and verify it is gone upstream and locally.
	if err := cache.DeleteBranch(context.Background(), "example", "feature/delete-me"); err != nil {
		t.Fatal(err)
	}
	output := runGit(t, "", "ls-remote", repository.upstream, "refs/heads/feature/delete-me")
	if output != "" {
		t.Fatalf("upstream ref still present after deletion: %s", output)
	}

	// A subsequent Ensure must not recreate the deleted branch.
	if _, err := cache.Ensure(context.Background(), "example"); err != nil {
		t.Fatal(err)
	}
	refs, err := cache.SnapshotRefs(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := refs["refs/heads/feature/delete-me"]; exists {
		t.Fatal("deleted branch reappeared after Ensure")
	}
}

func TestDeleteBranchSucceedsForNeverPublishedBranch(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	if _, err := cache.Ensure(context.Background(), "example"); err != nil {
		t.Fatal(err)
	}
	// Delete a branch that was never published upstream — should succeed
	// silently without attempting a remote push.
	if err := cache.DeleteBranch(context.Background(), "example", "feature/never-existed"); err != nil {
		t.Fatalf("deleting never-published branch failed: %v", err)
	}
}

func TestPromotionPushRejectsMovedNonFastForwardTarget(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	feature := repository.featureCommit(t, "feature/candidate", "candidate\n")
	runGit(t, repository.work, "push", ready.Path, feature+":refs/heads/feature/candidate")
	candidate, err := cache.PreparePromotion(context.Background(), "example", feature, "main", filepath.Join(t.TempDir(), "merge"))
	if err != nil {
		t.Fatal(err)
	}
	if !candidate.FastForward || candidate.MergedSHA != feature {
		t.Fatalf("candidate = %+v, want feature fast-forward", candidate)
	}

	repository.checkout(t, "main")
	repository.commit(t, "main moved", "main-moved\n")
	if err := cache.PushPromotion(context.Background(), "example", "main", candidate.MergedSHA); err == nil {
		t.Fatal("promotion unexpectedly overwrote moved upstream main")
	}
	if got := remoteRef(t, repository.upstream, "refs/heads/main"); got == feature {
		t.Fatal("non-fast-forward guard did not preserve moved upstream main")
	}
}

func TestAlreadyContainedPromotionRequiresTargetTreeCI(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	if _, err := cache.Ensure(context.Background(), "example"); err != nil {
		t.Fatal(err)
	}
	source := repository.initialSHA
	target := repository.commit(t, "target advanced", "target\n")

	candidate, err := cache.PreparePromotion(
		context.Background(), "example", source, "main", filepath.Join(t.TempDir(), "merge"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.FastForward || candidate.BaseSHA != target || candidate.MergedSHA != target {
		t.Fatalf("already-contained candidate = %+v, want target tree requiring CI", candidate)
	}
}

func TestMergedPromotionIsPinnedInCacheForCheckoutAndPush(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	feature := repository.featureCommit(t, "feature/merge", "feature\n")
	runGit(t, repository.work, "push", ready.Path, feature+":refs/heads/feature/merge")
	repository.checkout(t, "main")
	repository.commitFile(t, "target changed", "target.txt", "target\n")

	candidate, err := cache.PreparePromotion(context.Background(), "example", feature, "main", filepath.Join(t.TempDir(), "merge"))
	if err != nil {
		t.Fatal(err)
	}
	if candidate.FastForward || candidate.MergedSHA == feature {
		t.Fatalf("candidate = %+v, want a merge commit", candidate)
	}
	checkout := filepath.Join(t.TempDir(), "checkout")
	if err := cache.Checkout(context.Background(), "example", candidate.MergedSHA, checkout); err != nil {
		t.Fatalf("merged candidate was not persisted in cache: %v", err)
	}
	if err := cache.PushPromotion(context.Background(), "example", "main", candidate.MergedSHA); err != nil {
		t.Fatalf("push cached merged candidate: %v", err)
	}
	if got := remoteRef(t, repository.upstream, "refs/heads/main"); got != candidate.MergedSHA {
		t.Fatalf("upstream main = %s, want cached merge %s", got, candidate.MergedSHA)
	}
}

func TestReleaseAncestryAndTagSync(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	lock := cache.repoLock("example")
	lock.Lock()
	admission, err := cache.prepareReleaseAdmissionLocked(context.Background(), ready.Path, true)
	lock.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	reachable, err := cache.ReleaseReachable(context.Background(), "example", repository.initialSHA, admission.SHA)
	if err != nil || !reachable {
		t.Fatalf("main ancestry = %v, %v", reachable, err)
	}
	feature := repository.featureCommit(t, "feature/unmerged", "unmerged\n")
	runGit(t, repository.work, "push", ready.Path, feature+":refs/heads/feature/unmerged")
	receiveArgs := cache.serviceArgs(ReceivePack, ready.Path)
	receivePack := strings.Join(append([]string{cache.gitBinary}, receiveArgs[:len(receiveArgs)-1]...), " ")
	if output, pushErr := pushWithReceivePack(repository.work, ready.Path, receivePack, feature+":refs/heads/main"); pushErr != nil {
		t.Fatalf("default branch push = %v\n%s", pushErr, output)
	}
	reachable, err = cache.ReleaseReachable(context.Background(), "example", feature, admission.SHA)
	if err != nil || reachable {
		t.Fatalf("unpublished upstream ancestry = %v, %v", reachable, err)
	}
	runGit(t, repository.work, "tag", "-a", "v1.0.0", repository.initialSHA, "-m", "release")
	tagObject := runGit(t, repository.work, "rev-parse", "refs/tags/v1.0.0")
	runGit(t, repository.work, "push", ready.Path, "refs/tags/v1.0.0:refs/tags/v1.0.0")
	peeled, err := cache.PeelObject(context.Background(), "example", tagObject)
	if err != nil {
		t.Fatal(err)
	}
	if peeled.ObjectSHA != tagObject || peeled.CommitSHA != repository.initialSHA || peeled.ObjectSHA == peeled.CommitSHA || peeled.ObjectType != "tag" {
		t.Fatalf("peeled annotated tag = %+v", peeled)
	}
	if err := cache.SyncTag(context.Background(), "example", "v1.0.0", tagObject); err != nil {
		t.Fatal(err)
	}
	if got := remoteRef(t, repository.upstream, "refs/tags/v1.0.0"); got != tagObject {
		t.Fatalf("upstream tag = %s, want %s", got, tagObject)
	}
}

func TestConcurrentEnsureUsesOneCache(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	var wait sync.WaitGroup
	errors := make(chan error, 8)
	paths := make(chan string, 8)
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			ready, err := cache.Ensure(context.Background(), "example")
			if err != nil {
				errors <- err
				return
			}
			paths <- ready.Path
		}()
	}
	wait.Wait()
	close(errors)
	close(paths)
	for err := range errors {
		t.Fatal(err)
	}
	want := ""
	for path := range paths {
		if want == "" {
			want = path
		} else if path != want {
			t.Fatalf("concurrent cache paths differ: %q != %q", path, want)
		}
	}
}

func TestCommandLoggingRedactsURLCredentialsAndConfiguredValues(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	cache, err := New(Config{
		Root:     t.TempDir(),
		Upstream: func(string) (string, error) { return "unused", nil },
		Logger:   testLogger{&logs},
		Redact:   []string{"private-value"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := cache.redactedCommand([]string{"fetch", "https://token:private-value@example.invalid/repo.git?key=private-value"})
	if strings.Contains(got, "token") || strings.Contains(got, "private-value") || strings.Contains(got, "?key") {
		t.Fatalf("redacted command leaked credential material: %s", got)
	}
}

// TestMaterializeConvergesAfterPartialFailure proves that a public ref
// left stale by a failed materialize is correctly updated on the next
// Ensure. Without the durable manifest, the next refresh classifies the
// stale ref as client-owned divergence and skips it forever (#74).
func TestMaterializeConvergesAfterPartialFailure(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ctx := context.Background()

	ready, err := cache.Ensure(ctx, "example")
	if err != nil {
		t.Fatal(err)
	}
	if got := runGit(t, ready.Path, "rev-parse", "refs/heads/main"); got != repository.initialSHA {
		t.Fatalf("initial main = %s, want %s", got, repository.initialSHA)
	}

	// Advance upstream.
	newMainSHA := repository.commit(t, "advance main", "advanced main\n")

	// Simulate a partial materialize failure: fetch tracking refs but
	// leave public refs stale (as if materialize started but failed).
	lock := cache.repoLock("example")
	lock.Lock()
	if err := cache.fetchTracking(ctx, ready.Path); err != nil {
		lock.Unlock()
		t.Fatal(err)
	}
	lock.Unlock()

	// At this point tracking refs point to newMainSHA but public
	// refs/heads/main still points to initialSHA, and the manifest
	// records initialSHA. The next Ensure must converge.
	ready2, err := cache.Ensure(ctx, "example")
	if err != nil {
		t.Fatal(err)
	}
	got := runGit(t, ready2.Path, "rev-parse", "refs/heads/main")
	if got != newMainSHA {
		t.Fatalf("main after convergence = %s, want %s", got, newMainSHA)
	}
}

// TestMaterializeCrashRecoveryAcrossRestart simulates a crash between
// updating a public ref and writing the manifest, then verifies that a
// restarted cache converges correctly.
func TestMaterializeCrashRecoveryAcrossRestart(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	root := filepath.Join(t.TempDir(), "cache")
	cache := newTestCacheAt(t, root, repository.upstream)
	ctx := context.Background()

	ready, err := cache.Ensure(ctx, "example")
	if err != nil {
		t.Fatal(err)
	}

	// Advance upstream.
	midSHA := repository.commit(t, "mid advance", "mid\n")

	// Simulate: fetch tracking, update the public ref, but do NOT write
	// the manifest (as if the process crashed after the update-ref).
	lock := cache.repoLock("example")
	lock.Lock()
	if err := cache.fetchTracking(ctx, ready.Path); err != nil {
		lock.Unlock()
		t.Fatal(err)
	}
	if err := cache.updateRef(ctx, ready.Path, "refs/heads/main", midSHA, repository.initialSHA); err != nil {
		lock.Unlock()
		t.Fatal(err)
	}
	lock.Unlock()

	// Advance upstream again.
	finalSHA := repository.commit(t, "final advance", "final\n")

	// Simulate process restart with a fresh cache instance.
	restarted := newTestCacheAt(t, root, repository.upstream)
	ready2, err := restarted.Ensure(ctx, "example")
	if err != nil {
		t.Fatal(err)
	}

	// The restarted cache must converge main to the latest upstream.
	got := runGit(t, ready2.Path, "rev-parse", "refs/heads/main")
	if got != finalSHA {
		t.Fatalf("main after crash recovery = %s, want %s", got, finalSHA)
	}
}

// TestMaterializeSkipsUnchangedUpstream verifies that a repeated Ensure
// on an unchanged upstream does not error and produces the same result.
func TestMaterializeSkipsUnchangedUpstream(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ctx := context.Background()

	first, err := cache.Ensure(ctx, "example")
	if err != nil {
		t.Fatal(err)
	}
	second, err := cache.Ensure(ctx, "example")
	if err != nil {
		t.Fatal(err)
	}
	if second.Stale || second.Path != first.Path {
		t.Fatalf("repeated Ensure = %+v, want same stable path", second)
	}
	if got := runGit(t, second.Path, "rev-parse", "refs/heads/main"); got != repository.initialSHA {
		t.Fatalf("main after repeated Ensure = %s, want %s", got, repository.initialSHA)
	}
}

// TestMaterializePreservesClientModifiedRefs ensures that a public ref
// changed by a client push is not overwritten when upstream advances.
func TestMaterializePreservesClientModifiedRefs(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ctx := context.Background()

	ready, err := cache.Ensure(ctx, "example")
	if err != nil {
		t.Fatal(err)
	}

	// Client pushes a different commit to main via receive-pack.
	clientSHA := repository.featureCommit(t, "feature/client", "client content\n")
	receiveArgs := cache.serviceArgs(ReceivePack, ready.Path)
	receivePack := strings.Join(append([]string{cache.gitBinary}, receiveArgs[:len(receiveArgs)-1]...), " ")
	if output, pushErr := pushWithReceivePack(repository.work, ready.Path, receivePack, clientSHA+":refs/heads/main"); pushErr != nil {
		t.Fatalf("client push: %v\n%s", pushErr, output)
	}

	// Advance upstream.
	repository.checkout(t, "main")
	repository.commit(t, "upstream advance", "upstream advance\n")

	// Ensure must preserve the client's value on main.
	if _, err := cache.Ensure(ctx, "example"); err != nil {
		t.Fatal(err)
	}
	got := runGit(t, ready.Path, "rev-parse", "refs/heads/main")
	if got != clientSHA {
		t.Fatalf("client-modified main = %s, want preserved %s", got, clientSHA)
	}

	// A second Ensure should also preserve it (tests manifest stability).
	if _, err := cache.Ensure(ctx, "example"); err != nil {
		t.Fatal(err)
	}
	got = runGit(t, ready.Path, "rev-parse", "refs/heads/main")
	if got != clientSHA {
		t.Fatalf("client-modified main after second Ensure = %s, want preserved %s", got, clientSHA)
	}
}

// --- Public-ref cap tests for materialization ---

// TestMaterializeCapTruncatesExcessCreates verifies that when upstream has more
// refs than the cap, materialization truncates creates to stay at the ceiling.
func TestMaterializeCapTruncatesExcessCreates(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ctx := context.Background()

	ready, err := cache.Ensure(ctx, "example")
	if err != nil {
		t.Fatal(err)
	}
	// Seed client branches to fill up to the cap minus 2 (main + these = 4095).
	seedPublicBranches(t, ready.Path, "fill", maximumPublicRefs-2, repository.initialSHA)

	// Create 5 upstream branches — only 1 should be materialized (4095 + 1 = 4096).
	for i := range 5 {
		sha := repository.featureCommit(t, fmt.Sprintf("feature/upstream-%d", i), fmt.Sprintf("upstream %d\n", i))
		runGit(t, repository.work, "push", "origin", sha+fmt.Sprintf(":refs/heads/upstream-%d", i))
	}

	var logs bytes.Buffer
	cache.logger = testLogger{&logs}
	if _, err := cache.Ensure(ctx, "example"); err != nil {
		t.Fatal(err)
	}

	after, err := cache.SnapshotRefs(ctx, "example")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) > maximumPublicRefs {
		t.Fatalf("public refs after capped materialization = %d, want <= %d", len(after), maximumPublicRefs)
	}
	if !strings.Contains(logs.String(), "upstream materialization truncated") {
		t.Fatalf("expected truncation log line, got: %s", logs.String())
	}
}

// TestMaterializeDeletionFreesCapacityForCreates verifies that at the cap,
// an upstream delete+create cycle stays within bounds with deletes running first.
func TestMaterializeDeletionFreesCapacityForCreates(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ctx := context.Background()

	ready, err := cache.Ensure(ctx, "example")
	if err != nil {
		t.Fatal(err)
	}

	// Create an upstream branch, materialize it.
	retiredSHA := repository.featureCommit(t, "feature/retired", "retired\n")
	runGit(t, repository.work, "push", "origin", retiredSHA+":refs/heads/upstream-retired")
	if _, err := cache.Ensure(ctx, "example"); err != nil {
		t.Fatal(err)
	}

	// Fill to exactly the cap (main + upstream-retired + fill = 4096).
	seedPublicBranches(t, ready.Path, "fill-replacement", maximumPublicRefs-2, repository.initialSHA)

	// Upstream removes the retired branch and adds a replacement.
	replacementSHA := repository.featureCommit(t, "feature/replacement", "replacement\n")
	runGit(t, repository.work, "push", "origin", ":refs/heads/upstream-retired", replacementSHA+":refs/heads/upstream-replacement")

	var logs bytes.Buffer
	cache.logger = testLogger{&logs}
	refreshed, err := cache.Ensure(ctx, "example")
	if err != nil || refreshed.Stale {
		t.Fatalf("replacement at cap = %+v, %v", refreshed, err)
	}
	after, err := cache.SnapshotRefs(ctx, "example")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != maximumPublicRefs {
		t.Fatalf("public refs after replacement = %d, want %d", len(after), maximumPublicRefs)
	}
	if _, exists := after["refs/heads/upstream-retired"]; exists {
		t.Fatal("retired upstream ref survived materialization")
	}
	if after["refs/heads/upstream-replacement"] != replacementSHA {
		t.Fatal("replacement upstream ref was not materialized")
	}

	// Verify delete-before-create ordering in the log.
	deleteLog := `git ["update-ref" "-d" "refs/heads/upstream-retired"`
	createLog := `git ["update-ref" "refs/heads/upstream-replacement"`
	deleteAt := strings.Index(logs.String(), deleteLog)
	createAt := strings.Index(logs.String(), createLog)
	if deleteAt < 0 || createAt < 0 || deleteAt > createAt {
		t.Fatalf("materialization did not delete before create at the cap:\n%s", logs.String())
	}
}

// TestMaterializeConvergesAfterInterruptedDeletePhase injects a failing git
// runner mid-delete and verifies the next Ensure converges.
func TestMaterializeConvergesAfterInterruptedDeletePhase(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	root := filepath.Join(t.TempDir(), "cache")
	cache := newTestCacheAt(t, root, repository.upstream)
	ctx := context.Background()

	ready, err := cache.Ensure(ctx, "example")
	if err != nil {
		t.Fatal(err)
	}

	// Create 3 upstream branches, materialize them.
	for i := range 3 {
		sha := repository.featureCommit(t, fmt.Sprintf("feature/del-%d", i), fmt.Sprintf("del %d\n", i))
		runGit(t, repository.work, "push", "origin", sha+fmt.Sprintf(":refs/heads/del-%d", i))
	}
	if _, err := cache.Ensure(ctx, "example"); err != nil {
		t.Fatal(err)
	}

	// Upstream removes all 3.
	for i := range 3 {
		runGit(t, repository.work, "push", "origin", fmt.Sprintf(":refs/heads/del-%d", i))
	}

	// Simulate mid-delete failure: fetch tracking, delete only del-0, leave
	// del-1 and del-2, do NOT write manifest.
	lock := cache.repoLock("example")
	lock.Lock()
	if err := cache.fetchTracking(ctx, ready.Path); err != nil {
		lock.Unlock()
		t.Fatal(err)
	}
	del0SHA := runGit(t, ready.Path, "rev-parse", "refs/heads/del-0")
	if err := cache.deleteRef(ctx, ready.Path, "refs/heads/del-0", del0SHA); err != nil {
		lock.Unlock()
		t.Fatal(err)
	}
	lock.Unlock()

	// Restart and Ensure — must converge by deleting del-1 and del-2.
	restarted := newTestCacheAt(t, root, repository.upstream)
	if _, err := restarted.Ensure(ctx, "example"); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		ref := fmt.Sprintf("refs/heads/del-%d", i)
		if got := runGitOptional(t, ready.Path, "show-ref", "--verify", ref); got != "" {
			t.Fatalf("%s survived interrupted delete phase: %s", ref, got)
		}
	}
}

// TestMaterializeConvergesAfterInterruptedCreatePhase injects a failing git
// runner mid-create and verifies the next Ensure converges.
func TestMaterializeConvergesAfterInterruptedCreatePhase(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	root := filepath.Join(t.TempDir(), "cache")
	cache := newTestCacheAt(t, root, repository.upstream)
	ctx := context.Background()

	ready, err := cache.Ensure(ctx, "example")
	if err != nil {
		t.Fatal(err)
	}

	// Upstream adds 3 new branches.
	expectedSHAs := make(map[string]string)
	for i := range 3 {
		ref := fmt.Sprintf("refs/heads/create-%d", i)
		sha := repository.featureCommit(t, fmt.Sprintf("feature/create-%d", i), fmt.Sprintf("create %d\n", i))
		runGit(t, repository.work, "push", "origin", sha+":"+ref)
		expectedSHAs[ref] = sha
	}

	// Simulate mid-create failure: fetch tracking, create only create-0, leave
	// create-1 and create-2 uncreated, do NOT write manifest.
	lock := cache.repoLock("example")
	lock.Lock()
	if err := cache.fetchTracking(ctx, ready.Path); err != nil {
		lock.Unlock()
		t.Fatal(err)
	}
	if err := cache.updateRef(ctx, ready.Path, "refs/heads/create-0", expectedSHAs["refs/heads/create-0"], ""); err != nil {
		lock.Unlock()
		t.Fatal(err)
	}
	lock.Unlock()

	// Restart and Ensure — must converge by creating create-1 and create-2.
	restarted := newTestCacheAt(t, root, repository.upstream)
	if _, err := restarted.Ensure(ctx, "example"); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		ref := fmt.Sprintf("refs/heads/create-%d", i)
		got := runGitOptional(t, ready.Path, "rev-parse", ref)
		if got == "" {
			t.Fatalf("%s was not created after convergence", ref)
		}
		if got != expectedSHAs[ref] {
			t.Fatalf("%s = %s, want %s", ref, got, expectedSHAs[ref])
		}
	}
}

// TestReaderConsistencyPerRefAtomicDuringMaterialize verifies that during
// materialization, each individual ref is either at the old SHA or the new SHA,
// never torn, and never pointing at a missing object (objects precede refs
// because fetch precedes materialize).
func TestReaderConsistencyPerRefAtomicDuringMaterialize(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ctx := context.Background()

	ready, err := cache.Ensure(ctx, "example")
	if err != nil {
		t.Fatal(err)
	}
	oldMainSHA := repository.initialSHA

	// Advance upstream.
	newMainSHA := repository.commit(t, "advance for reader test", "new content\n")

	// Fetch objects (simulating the first half of refresh).
	lock := cache.repoLock("example")
	lock.Lock()
	if err := cache.fetchTracking(ctx, ready.Path); err != nil {
		lock.Unlock()
		t.Fatal(err)
	}
	lock.Unlock()

	// Between fetch and materialize, verify:
	// 1. The new object exists (objects precede refs).
	objType := runGitOptional(t, ready.Path, "cat-file", "-t", newMainSHA)
	if objType != "commit" {
		t.Fatalf("new object should be reachable after fetch; cat-file -t = %q", objType)
	}
	// 2. The public ref still has the old SHA (refs not yet updated).
	currentRef := runGit(t, ready.Path, "rev-parse", "refs/heads/main")
	if currentRef != oldMainSHA {
		t.Fatalf("main before materialize = %s, want old %s", currentRef, oldMainSHA)
	}

	// Run materialize.
	if _, err := cache.Ensure(ctx, "example"); err != nil {
		t.Fatal(err)
	}
	// 3. After materialize, ref is at the new SHA.
	currentRef = runGit(t, ready.Path, "rev-parse", "refs/heads/main")
	if currentRef != newMainSHA {
		t.Fatalf("main after materialize = %s, want new %s", currentRef, newMainSHA)
	}
	// 4. Object is still valid (ref never points at a missing object).
	objType = runGitOptional(t, ready.Path, "cat-file", "-t", newMainSHA)
	if objType != "commit" {
		t.Fatalf("object behind ref is not valid: cat-file -t = %q", objType)
	}
}

// TestLegacyOverflowRecoversMaterializationNotBricked verifies that a repo
// already above the public-ref cap is not bricked: Ensure still works (with
// truncation), and the materializer does not try to add more refs.
func TestLegacyOverflowRecoversMaterializationNotBricked(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ctx := context.Background()

	ready, err := cache.Ensure(ctx, "example")
	if err != nil {
		t.Fatal(err)
	}
	// Seed client branches past the cap.
	seedPublicBranches(t, ready.Path, "overflow", maximumPublicRefs+100, repository.initialSHA)

	// Advance upstream.
	newSHA := repository.commit(t, "advance while over cap", "advanced\n")

	// Ensure should still work — materialize truncates, does not error.
	var logs bytes.Buffer
	cache.logger = testLogger{&logs}
	refreshed, err := cache.Ensure(ctx, "example")
	if err != nil {
		t.Fatalf("Ensure with legacy overflow: %v", err)
	}
	if refreshed.Stale {
		t.Fatal("Ensure with legacy overflow returned stale")
	}
	// Main should be updated since the materializer owns it.
	got := runGit(t, ready.Path, "rev-parse", "refs/heads/main")
	if got != newSHA {
		t.Fatalf("main after overflow Ensure = %s, want %s", got, newSHA)
	}
}

func TestLsRemoteHeadsSucceedsForReachableUpstream(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	if _, err := cache.Ensure(context.Background(), "example"); err != nil {
		t.Fatal(err)
	}
	count, err := cache.LsRemoteHeads(context.Background(), "example")
	if err != nil {
		t.Fatalf("LsRemoteHeads: %v", err)
	}
	if count < 1 {
		t.Fatalf("expected at least 1 branch, got %d", count)
	}
}

func TestLsRemoteHeadsFailsForUnreachableUpstream(t *testing.T) {
	t.Parallel()
	cache, err := New(Config{
		Root:           t.TempDir(),
		CommandTimeout: 5 * time.Second,
		Upstream: func(repo string) (string, error) {
			return "/nonexistent/upstream/" + repo + ".git", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = cache.LsRemoteHeads(context.Background(), "example")
	if err == nil {
		t.Fatal("expected error for unreachable upstream")
	}
}

type testLogger struct{ writer *bytes.Buffer }

func (l testLogger) Printf(format string, args ...any) {
	fmt.Fprintf(l.writer, format, args...)
}

type testRepository struct {
	upstream      string
	work          string
	initialSHA    string
	defaultBranch string
}

func newTestRepository(t *testing.T) *testRepository {
	return newTestRepositoryWithBranch(t, "main")
}

func newTestRepositoryWithBranch(t *testing.T, branch string) *testRepository {
	t.Helper()
	root := t.TempDir()
	upstream := filepath.Join(root, "upstream.git")
	work := filepath.Join(root, "work")
	runGit(t, "", "init", "--bare", "--initial-branch="+branch, upstream)
	runGit(t, "", "init", "--initial-branch="+branch, work)
	runGit(t, work, "config", "user.name", "Test")
	runGit(t, work, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("initial\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "file.txt")
	runGit(t, work, "commit", "-m", "initial")
	runGit(t, work, "remote", "add", "origin", upstream)
	runGit(t, work, "push", "-u", "origin", branch)
	return &testRepository{upstream: upstream, work: work, initialSHA: runGit(t, work, "rev-parse", "HEAD"), defaultBranch: branch}
}

func (r *testRepository) commit(t *testing.T, message, contents string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.work, "file.txt"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, r.work, "add", "file.txt")
	runGit(t, r.work, "commit", "-m", message)
	runGit(t, r.work, "push", "origin", "HEAD:"+r.defaultBranch)
	return runGit(t, r.work, "rev-parse", "HEAD")
}

func (r *testRepository) commitFile(t *testing.T, message, name, contents string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.work, name), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, r.work, "add", name)
	runGit(t, r.work, "commit", "-m", message)
	runGit(t, r.work, "push", "origin", "HEAD:"+r.defaultBranch)
	return runGit(t, r.work, "rev-parse", "HEAD")
}

func (r *testRepository) featureCommit(t *testing.T, branch, contents string) string {
	t.Helper()
	runGit(t, r.work, "checkout", "-B", branch, r.initialSHA)
	if err := os.WriteFile(filepath.Join(r.work, "file.txt"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, r.work, "add", "file.txt")
	runGit(t, r.work, "commit", "-m", branch)
	return runGit(t, r.work, "rev-parse", "HEAD")
}

func (r *testRepository) checkout(t *testing.T, branch string) {
	t.Helper()
	runGit(t, r.work, "checkout", branch)
}

func newTestCache(t *testing.T, upstream string) *Cache {
	t.Helper()
	cache, err := New(Config{
		Root:           filepath.Join(t.TempDir(), "cache"),
		CommandTimeout: 10 * time.Second,
		Upstream: func(repo string) (string, error) {
			if repo != "example" {
				return "", fmt.Errorf("unknown repo %s", repo)
			}
			return upstream, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return cache
}

func remoteRef(t *testing.T, remote, ref string) string {
	t.Helper()
	output := runGit(t, "", "ls-remote", remote, ref)
	fields := strings.Fields(output)
	if len(fields) != 2 || fields[1] != ref {
		t.Fatalf("ls-remote %s = %q", ref, output)
	}
	return fields[0]
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	if err != nil {
		// symbolic-ref -q intentionally uses exit status one for detached HEAD.
		if len(args) >= 2 && args[0] == "symbolic-ref" && args[1] == "-q" {
			return ""
		}
		t.Fatalf("git %q failed: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}
