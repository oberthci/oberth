package gitcache

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/testtimeout"
)

// A stock OpenSSH forge speaks protocol v0, which never advertises an unborn
// HEAD. lsrefs.unborn=ignore makes a local upstream answer the same way.
// Once Oberth has synced any branch there, the upstream has refs but no
// HEAD; that must still read as an unborn default branch so the first
// promotion can create it, instead of an "infrastructure" error forever.
func TestUnbornDefaultBranchWithSyncedBranchesAllowsFirstPromotion(t *testing.T) {
	t.Parallel()
	for _, head := range []string{"main", "trunk"} {
		t.Run("upstream HEAD "+head, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), testtimeout.Start(60*time.Second))
			defer cancel()
			root := t.TempDir()
			upstream := filepath.Join(root, "upstream.git")
			runGit(t, "", "init", "--bare", "--initial-branch="+head, upstream)
			runGit(t, upstream, "config", "lsrefs.unborn", "ignore")
			cache, err := newBackendTestCache(t, Config{
				Root:           filepath.Join(root, "cache"),
				CommandTimeout: testtimeout.Start(10 * time.Second),
				Upstream:       func(string) (string, error) { return upstream, nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			ready, err := cache.Ensure(ctx, "demo")
			if err != nil || ready.DefaultBranch != "main" {
				t.Fatalf("Ensure on an empty upstream = %+v, %v; want default main", ready, err)
			}

			work := filepath.Join(root, "work")
			runGit(t, "", "init", "--initial-branch=hello", work)
			if err := os.WriteFile(filepath.Join(work, "hello.sh"), []byte("echo hello\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, work, "add", "hello.sh")
			runGit(t, work, "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-m", "hello")
			source := runGit(t, work, "rev-parse", "HEAD")
			runGit(t, work, "push", upstream, "HEAD:refs/heads/hello")

			ready, err = cache.Ensure(ctx, "demo")
			if err != nil || ready.Stale || ready.DefaultBranch != "main" {
				t.Fatalf("Ensure with a synced branch and an unborn HEAD = %+v, %v; want fresh, default main", ready, err)
			}
			if _, _, exists, err := cache.ReadDefaultBlob(WithFreshPolicy(ctx), "demo", ".oberth/promotion.yaml", 4096); err != nil || exists {
				t.Fatalf("policy on an unborn default branch = exists %v, %v; want absent without error", exists, err)
			}
			admission, err := cache.prepareReleaseAdmissionLocked(ctx, ready.Path, true)
			if err != nil || admission.SHA != "" {
				t.Fatalf("release admission on an unborn default = %+v, %v; want empty", admission, err)
			}

			candidate, err := cache.PreparePromotion(ctx, "demo", source, "main", filepath.Join(root, "merge"))
			if err != nil || !candidate.TargetUnborn || candidate.MergedSHA != source {
				t.Fatalf("first promotion = %+v, %v; want the unborn target created from the source", candidate, err)
			}
			if err := cache.PushPromotion(ctx, "demo", "main", source); err != nil {
				t.Fatalf("push first promotion: %v", err)
			}
			if got := runGit(t, upstream, "rev-parse", "refs/heads/main"); got != source {
				t.Fatalf("upstream main = %s, want %s", got, source)
			}
			ready, err = cache.Ensure(ctx, "demo")
			if err != nil || ready.Stale || ready.DefaultBranch != "main" {
				t.Fatalf("Ensure after the first promotion = %+v, %v", ready, err)
			}
		})
	}
}

func TestHeadAdvertisedSeparatesUnbornFromUnnamedHead(t *testing.T) {
	t.Parallel()
	if headAdvertised("") || headAdvertised("abc\trefs/heads/hello\n") {
		t.Fatal("an advertisement without HEAD reads as a resolved HEAD")
	}
	if !headAdvertised("0123456789012345678901234567890123456789\tHEAD\n") {
		t.Fatal("a resolved HEAD row was not detected")
	}
	if !strings.Contains(staleReason(errors.Join(errors.New("x"), ErrUpstreamDefaultBranch)), "default branch") {
		t.Fatal("stale reason does not name the default-branch cause")
	}
	if staleReason(errors.New("connection refused")) != "upstream unavailable" {
		t.Fatal("a transport failure changed its stale reason")
	}
}

func TestMissingRepositoryHintSuggestsHomeRelativeUpstream(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cache, err := newBackendTestCache(t, Config{Root: filepath.Join(root, "cache"), CommandTimeout: testtimeout.Start(10 * time.Second),
		Upstream: func(string) (string, error) { return "", errors.New("unused") }})
	if err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(root, "bare.git")
	runGit(t, "", "init", "--bare", bare)
	runGit(t, bare, "remote", "add", "upstream", "ssh://git@forge.example:2222/acme/demo.git")
	refused := errors.New("exit status 128: fatal: '/acme/demo.git' does not appear to be a git repository")
	hint := cache.missingRepositoryHint(context.Background(), bare, refused)
	home := (&url.URL{Scheme: "ssh", User: url.User("git"), Host: "forge.example:2222", Path: "/~/acme"}).String()
	if !strings.Contains(hint, "/acme/demo.git") || !strings.Contains(hint, home) {
		t.Fatalf("hint = %q, want the refused path and %s", hint, home)
	}
	if got := cache.missingRepositoryHint(context.Background(), bare, errors.New("connection refused")); got != "" {
		t.Fatalf("hint for a transport failure = %q", got)
	}
}

// A default branch that existed upstream and then disappears beside other
// refs was deleted, not unborn: its policy must not lapse and the next push
// or promotion must not recreate it ungoverned. The cache is served stale
// with the configuration cause, policy reads fail, and release admission
// refuses. A never-born default beside other refs stays unborn (above).
func TestDeletedDefaultBranchIsNotReadAsUnborn(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testtimeout.Start(60*time.Second))
	defer cancel()
	root := t.TempDir()
	upstream := filepath.Join(root, "upstream.git")
	runGit(t, "", "init", "--bare", "--initial-branch=main", upstream)
	work := filepath.Join(root, "work")
	runGit(t, "", "init", "--initial-branch=main", work)
	if err := os.WriteFile(filepath.Join(work, "policy.yaml"), []byte("policy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "policy.yaml")
	runGit(t, work, "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-m", "policy")
	runGit(t, work, "push", upstream, "main", "main:feature")
	cache, err := newBackendTestCache(t, Config{
		Root:           filepath.Join(root, "cache"),
		CommandTimeout: testtimeout.Start(10 * time.Second),
		Upstream:       func(string) (string, error) { return upstream, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := cache.Ensure(ctx, "demo")
	if err != nil || ready.Stale {
		t.Fatalf("Ensure = %+v, %v", ready, err)
	}
	if _, _, exists, err := cache.ReadDefaultBlob(WithFreshPolicy(ctx), "demo", "policy.yaml", 4096); err != nil || !exists {
		t.Fatalf("policy on a born default = exists %v, %v", exists, err)
	}

	runGit(t, upstream, "update-ref", "-d", "refs/heads/main")
	ready, err = cache.Ensure(ctx, "demo")
	if err != nil || !ready.Stale || !strings.Contains(ready.StaleReason, "default branch") {
		t.Fatalf("Ensure after the default branch was deleted = %+v, %v; want stale with the default-branch cause", ready, err)
	}
	if sha, _, _, err := cache.ReadDefaultBlob(WithFreshPolicy(ctx), "demo", "policy.yaml", 4096); err == nil {
		t.Fatalf("policy after the default branch was deleted = %q without error; want a refusal, never an absent policy", sha)
	}
	if admission, err := cache.prepareReleaseAdmissionLocked(ctx, ready.Path, true); err == nil {
		t.Fatalf("release admission after the default branch was deleted = %+v; want a refusal", admission)
	}

	// Restoring the branch on the forge restores its authority.
	runGit(t, work, "push", upstream, "main")
	ready, err = cache.Ensure(ctx, "demo")
	if err != nil || ready.Stale {
		t.Fatalf("Ensure after the default branch was restored = %+v, %v", ready, err)
	}
	if _, _, exists, err := cache.ReadDefaultBlob(WithFreshPolicy(ctx), "demo", "policy.yaml", 4096); err != nil || !exists {
		t.Fatalf("policy after restore = exists %v, %v", exists, err)
	}
}
