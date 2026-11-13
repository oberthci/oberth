package gitcache

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestModuleRepositoryLockWaitHonorsContext(t *testing.T) {
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	lock := cache.repoLock(ready.Path)
	lock.Lock()
	defer lock.Unlock()
	operations := map[string]func(context.Context) error{
		"list": func(ctx context.Context) error { _, err := cache.ListTags(ctx, "example"); return err },
		"mod": func(ctx context.Context) error {
			_, err := cache.ShowFile(ctx, "example", "v1.0.0", "go.mod")
			return err
		},
		"zip": func(ctx context.Context) error { return cache.ArchiveTar(ctx, "example", "v1.0.0", io.Discard) },
	}
	for name, read := range operations {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
			defer cancel()
			if err := read(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("held repository lock ignored deadline: %v", err)
			}
		})
	}
}

func TestModuleCatalogQualificationUsesCallerDeadline(t *testing.T) {
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	cache.qualify = func(string) (RepoQualification, error) {
		t.Fatal("module read used the detached legacy qualifier")
		return RepoQualification{}, nil
	}
	cache.qualifyContext = func(ctx context.Context, _ string) (RepoQualification, error) {
		<-ctx.Done()
		return RepoQualification{}, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if _, err := cache.ListTags(ctx, "example"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("catalog wait escaped the proxy read budget: %v", err)
	}
}

// Counterfactual: with the old 10ms TryLock poller, the ordinary Lock waiters
// hand off to one another and ListTags times out under this sustained load.
func TestModuleListTagsIsNotStarvedByOrdinaryRepositoryUsers(t *testing.T) {
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	lock := cache.repoLock(ready.Path)
	stop := make(chan struct{})
	started := make(chan struct{}, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			lock.Lock()
			started <- struct{}{}
			for {
				time.Sleep(20 * time.Millisecond)
				lock.Unlock()
				select {
				case <-stop:
					return
				default:
				}
				lock.Lock()
			}
		})
	}
	<-started
	<-started
	ctx, cancel := context.WithTimeout(t.Context(), 750*time.Millisecond)
	defer cancel()
	_, err = cache.ListTags(ctx, "example")
	close(stop)
	workers.Wait()
	if err != nil {
		t.Fatalf("ordinary users starved deadline-aware ListTags: %v", err)
	}
}

func TestPinnedModuleReadClassifiesOnlyConfirmedMissingCommit(t *testing.T) {
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	if _, err := cache.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	missing := strings.Repeat("f", 40)
	if _, err := cache.ShowFile(t.Context(), "example", missing, "go.mod"); !errors.Is(err, ErrModuleSourceMissing) {
		t.Fatalf("missing pinned blob not classified: %v", err)
	}
	if err := cache.ArchiveTar(t.Context(), "example", missing, io.Discard); !errors.Is(err, ErrModuleSourceMissing) {
		t.Fatalf("missing pinned archive not classified: %v", err)
	}
	if _, err := cache.ShowFile(t.Context(), "example", repository.initialSHA, "nonexistent-file"); !errors.Is(err, ErrModuleFileMissing) || errors.Is(err, ErrModuleSourceMissing) {
		t.Fatalf("existing source misclassified missing: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := cache.ArchiveTar(ctx, "example", missing, io.Discard); !errors.Is(err, context.Canceled) || errors.Is(err, ErrModuleSourceMissing) {
		t.Fatalf("cancellation became permanent: %v", err)
	}
}

func TestModuleFileReadFailureDoesNotConfuseBrokenStorageWithAbsentPath(t *testing.T) {
	repo := newTestRepository(t)
	cache := newTestCache(t, repo.upstream)
	ready, err := cache.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	blob := runGitStdin(t, ready.Path, "missing-storage-fixture\n", "hash-object", "-w", "--stdin")
	tree := runGitStdin(t, ready.Path, "100644 blob "+blob+"\tgo.mod\n", "mktree")
	source := runGit(t, ready.Path, "-c", "user.name=test", "-c", "user.email=test@example.test", "commit-tree", tree, "-m", "storage fixture")
	if err := os.Remove(filepath.Join(ready.Path, "objects", blob[:2], blob[2:])); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.ShowFile(t.Context(), "example", source, "go.mod"); err == nil || errors.Is(err, ErrModuleFileMissing) || errors.Is(err, ErrModuleSourceMissing) {
		t.Fatalf("missing stored blob became a terminal absent path: %v", err)
	}
}
