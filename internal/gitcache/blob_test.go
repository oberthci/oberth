package gitcache

// Extracted verbatim from upstream PR #13 (commit 1e010341) together with
// blob.go: the ReadBlob tests only. The TagSHA and fragment-resolution tests
// stay with PR #13.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func commitFragment(t *testing.T, repository *testRepository, message, contents string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repository.work, ".oberth"), 0o700); err != nil {
		t.Fatal(err)
	}
	return repository.commitFile(t, message, ".oberth/fragment.yaml", contents)
}

func TestReadBlobReturnsTheFileAtThatCommit(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ctx := context.Background()

	const body = "apiVersion: argoproj.io/v1alpha1\nkind: Workflow\n"
	sha := commitFragment(t, repository, "fragment", body)
	commitFragment(t, repository, "later", "changed after the tag\n")
	if _, err := cache.Ensure(ctx, "example"); err != nil {
		t.Fatal(err)
	}

	got, err := cache.ReadBlob(ctx, "example", sha, ".oberth/fragment.yaml", 1<<20)
	if err != nil {
		t.Fatalf("ReadBlob: %v", err)
	}
	if string(got) != body {
		t.Fatalf("ReadBlob returned %q, want the content at the pinned commit %q", got, body)
	}
}

func TestReadBlobReturnsAFileLargerThanTheCaptureCeiling(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ctx := context.Background()

	body := "apiVersion: argoproj.io/v1alpha1\nkind: Workflow\n" +
		strings.Repeat("# filler filler filler filler filler filler\n", 4000)
	if len(body) <= maxCapturedOutput {
		t.Fatalf("fixture is %d bytes, not above the %d byte capture ceiling", len(body), maxCapturedOutput)
	}
	sha := commitFragment(t, repository, "big fragment", body)
	if _, err := cache.Ensure(ctx, "example"); err != nil {
		t.Fatal(err)
	}

	got, err := cache.ReadBlob(ctx, "example", sha, ".oberth/fragment.yaml", 1<<20)
	if err != nil {
		t.Fatalf("ReadBlob: %v", err)
	}
	if len(got) != len(body) {
		t.Fatalf("ReadBlob returned %d bytes, want %d; the read is truncating", len(got), len(body))
	}
	if string(got) != body {
		t.Fatal("ReadBlob returned different content than was committed")
	}
}

func TestReadBlobRefusesAFileOverTheLimit(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ctx := context.Background()

	body := strings.Repeat("x", 5000)
	sha := commitFragment(t, repository, "oversize", body)
	if _, err := cache.Ensure(ctx, "example"); err != nil {
		t.Fatal(err)
	}

	if _, err := cache.ReadBlob(ctx, "example", sha, ".oberth/fragment.yaml", 1000); err == nil {
		t.Fatal("ReadBlob returned a blob larger than the caller's limit instead of refusing it")
	}
}

func TestReadBlobRefusesAnUnsafePath(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ctx := context.Background()
	sha := commitFragment(t, repository, "fragment", "kind: Workflow\n")
	if _, err := cache.Ensure(ctx, "example"); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"",
		"../outside.yaml",
		"/etc/passwd",
		".oberth/../../escape.yaml",
		"--output=/tmp/pwned",
		".oberth/fragment.yaml\nrm -rf /",
	} {
		if _, err := cache.ReadBlob(ctx, "example", sha, path, 1<<20); err == nil {
			t.Fatalf("ReadBlob admitted the path %q", path)
		}
	}
}

func TestReadBlobRefusesAMissingFile(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ctx := context.Background()
	sha := repository.commitFile(t, "no fragment", "other.txt", "nothing here\n")
	if _, err := cache.Ensure(ctx, "example"); err != nil {
		t.Fatal(err)
	}

	if _, err := cache.ReadBlob(ctx, "example", sha, ".oberth/fragment.yaml", 1<<20); err == nil {
		t.Fatal("ReadBlob succeeded for a file the commit does not contain")
	}
}

func TestReadBlobRefusesAnInvalidSHA(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ctx := context.Background()
	if _, err := cache.Ensure(ctx, "example"); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.ReadBlob(ctx, "example", "HEAD", ".oberth/fragment.yaml", 1<<20); err == nil {
		t.Fatal("ReadBlob accepted a revision expression instead of a resolved SHA")
	}
}

func TestReadBlobDoesNotReachOutsideTheCacheRoot(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ctx := context.Background()
	sha := commitFragment(t, repository, "fragment", "kind: Workflow\n")
	if _, err := cache.Ensure(ctx, "example"); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.ReadBlob(ctx, "../../"+outside, sha, ".oberth/fragment.yaml", 1<<20); err == nil {
		t.Fatal("ReadBlob followed a repository path outside the cache root")
	}
}
