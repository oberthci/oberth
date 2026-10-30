package gitcache

import (
	"archive/tar"
	"bytes"
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

func TestModuleSnapshotPinsAnnotatedTagAndArchive(t *testing.T) {
	repository := newTestRepository(t)
	want := []byte("module go.example.test/snapshot\n\ngo 1.22\n")
	if err := os.WriteFile(filepath.Join(repository.work, "go.mod"), want, 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository.work, "add", "go.mod")
	runGit(t, repository.work, "commit", "-m", "module snapshot")
	runGit(t, repository.work, "tag", "-a", "v1.0.0", "-m", "annotated release")
	runGit(t, repository.work, "push", repository.upstream, "refs/tags/v1.0.0")
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	source, timestamp, err := cache.ModuleTagSnapshot(t.Context(), "example", "v1.0.0")
	if err != nil || timestamp.IsZero() || source != strings.TrimSpace(runGit(t, ready.Path, "rev-parse", "refs/tags/v1.0.0")) {
		t.Fatalf("snapshot lost exact annotated tag identity: %s %s %v", source, timestamp, err)
	}
	// Simulate a tag changing between independently locked proxy operations.
	runGit(t, ready.Path, "update-ref", "refs/tags/v1.0.0", repository.initialSHA)
	got, err := cache.ShowFile(t.Context(), "example", source, "go.mod")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("pinned show followed a moved tag: %q %v", got, err)
	}
	var archive bytes.Buffer
	if err := cache.ArchiveTar(t.Context(), "example", source, &archive); err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(&archive)
	found := false
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "go.mod" {
			body, err := io.ReadAll(reader)
			if err != nil || !bytes.Equal(body, want) {
				t.Fatal("pinned archive changed", err)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("pinned archive lost go.mod")
	}
}

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
		"time": func(ctx context.Context) error { _, err := cache.TagTime(ctx, "example", "v1.0.0"); return err },
		"mod": func(ctx context.Context) error {
			_, err := cache.ShowFile(ctx, "example", "v1.0.0", "go.mod")
			return err
		},
		"zip": func(ctx context.Context) error { return cache.ArchiveTar(ctx, "example", "v1.0.0", io.Discard) },
		"snapshot": func(ctx context.Context) error {
			_, _, err := cache.ModuleTagSnapshot(ctx, "example", "v1.0.0")
			return err
		},
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
