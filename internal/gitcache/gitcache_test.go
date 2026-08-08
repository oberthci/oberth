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
	if peeled.ObjectSHA != tagObject || peeled.CommitSHA != repository.initialSHA || peeled.ObjectSHA == peeled.CommitSHA {
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
