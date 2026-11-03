package gitcache

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/testtimeout"
)

type advertisedWriter struct {
	once  sync.Once
	ready chan struct{}
}

func (w *advertisedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.ready) })
	return len(p), nil
}

func TestStalledUploadAllowsConcurrentFetchCheckoutAndModuleRead(t *testing.T) {
	r := newTestRepository(t)
	c := newTestCache(t, r.upstream)
	if _, err := c.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	input, writer := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	advertised := &advertisedWriter{ready: make(chan struct{})}
	go func() { done <- c.Serve(ctx, "example", UploadPack, "", input, advertised, io.Discard) }()
	t.Cleanup(func() {
		cancel()
		_ = writer.Close()
		_ = input.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("stalled upload did not stop")
		}
	})
	select {
	case <-advertised.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("no advertisement")
	}
	operations := map[string]func(context.Context) error{
		"fetch": func(ctx context.Context) error {
			if _, err := c.EnsureForFetch(ctx, "example"); err != nil {
				return err
			}
			return c.Serve(ctx, "example", UploadPack, "", strings.NewReader("0000"), io.Discard, io.Discard)
		},
		"checkout": func(ctx context.Context) error {
			return c.Checkout(ctx, "example", r.initialSHA, filepath.Join(t.TempDir(), "checkout"))
		},
		"module": func(ctx context.Context) error {
			_, err := c.ShowFile(ctx, "example", r.initialSHA, "file.txt")
			return err
		},
	}
	for name, op := range operations {
		t.Run(name, func(t *testing.T) {
			// The restricted static runtime can spend several seconds reaping
			// each Git child. A locked reader still cannot finish before cleanup.
			ctx, cancel := context.WithTimeout(t.Context(), testtimeout.Start(30*time.Second))
			defer cancel()
			completed := make(chan error, 1)
			go func() { completed <- op(ctx) }()
			select {
			case err := <-completed:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("stalled upload blocks independent read")
			}
			select {
			case err := <-done:
				done <- err
				t.Fatalf("upload ended before its input was released: %v", err)
			default:
			}
		})
	}
}
func TestRepositoryWaitsHonorContext(t *testing.T) {
	r := newTestRepository(t)
	c := newTestCache(t, r.upstream)
	ready, err := c.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	operations := map[string]func(context.Context) error{
		"ensure": func(ctx context.Context) error { _, err := c.Ensure(ctx, "example"); return err },
		"serve": func(ctx context.Context) error {
			return c.Serve(ctx, "example", UploadPack, "", strings.NewReader("0000"), io.Discard, io.Discard)
		},
		"receive": func(ctx context.Context) error {
			return c.Receive(ctx, "example", "actor", "", nil, io.Discard, io.Discard, ReceiveHandlerFunc(func(context.Context, ReceiveEvent) error { return nil }))
		},
		"promotion": func(ctx context.Context) error {
			_, err := c.PreparePromotion(ctx, "example", r.initialSHA, "main", filepath.Join(t.TempDir(), "merge"))
			return err
		},
	}
	for name, op := range operations {
		t.Run(name, func(t *testing.T) {
			lock := c.repoLock(ready.Path)
			lock.Lock()
			ctx, cancel := context.WithCancel(t.Context())
			completed := make(chan error, 1)
			go func() { completed <- op(ctx) }()
			cancel()
			select {
			case err := <-completed:
				lock.Unlock()
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(250 * time.Millisecond):
				lock.Unlock()
				<-completed
				t.Fatal("lock ignores cancellation")
			}
		})
	}
}

func TestStalledReceiveAllowsReadersWithoutRefreshingItsSnapshot(t *testing.T) {
	r := newTestRepository(t)
	c := newTestCache(t, r.upstream)
	ready, err := c.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- c.receiveTransaction(t.Context(), "example", ready.Path, "actor", func() error { close(started); <-release; return nil }, ReceiveHandlerFunc(func(context.Context, ReceiveEvent) error { return nil }))
	}()
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	<-started
	// An upstream advance must not be attributed to this actor's still-open receive.
	r.commit(t, "upstream advanced", "new\\n")
	ctx, cancel := context.WithTimeout(t.Context(), testtimeout.Start(30*time.Second))
	defer cancel()
	readDone := make(chan error, 1)
	go func() {
		_, err := c.EnsureForFetch(ctx, "example")
		if err == nil {
			err = c.Checkout(ctx, "example", r.initialSHA, filepath.Join(t.TempDir(), "read"))
		}
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("stalled receive blocked readers")
	}
	if got := runGit(t, ready.Path, "rev-parse", "refs/heads/main"); got != r.initialSHA {
		t.Fatalf("unfinalized receive was refreshed to %s", got)
	}
}
