package gitcache

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

// The pinned semaphore consults Done on entry to each acquisition. Observing
// that call separates worker startup from the cancellation response budget.
type lockWaitContext struct {
	context.Context
	acquisitions atomic.Int32
	entered      chan struct{}
}

func (ctx *lockWaitContext) Done() <-chan struct{} {
	if ctx.acquisitions.Add(-1) == 0 {
		close(ctx.entered)
	}
	return ctx.Context.Done()
}

func TestRepositoryWaitsHonorContext(t *testing.T) {
	r := newTestRepository(t)
	c := newTestCache(t, r.upstream)
	ready, err := c.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	// Filesystem setup must finish before starting a cancellation measurement.
	promotionWorkspace := filepath.Join(t.TempDir(), "merge")
	operations := map[string]func(context.Context) error{
		"ensure": func(ctx context.Context) error { _, err := c.Ensure(ctx, "example"); return err },
		"serve": func(ctx context.Context) error {
			return c.Serve(ctx, "example", UploadPack, "", strings.NewReader("0000"), io.Discard, io.Discard)
		},
		"receive": func(ctx context.Context) error {
			return c.Receive(ctx, "example", "actor", "", nil, io.Discard, io.Discard, ReceiveHandlerFunc(func(context.Context, ReceiveEvent) error { return nil }))
		},
		"promotion": func(ctx context.Context) error {
			_, err := c.PreparePromotion(ctx, "example", r.initialSHA, "main", promotionWorkspace)
			return err
		},
	}
	for name, op := range operations {
		t.Run(name, func(t *testing.T) {
			for _, alreadyCancelled := range []bool{true, false} {
				phase := "contended"
				if alreadyCancelled {
					phase = "already-cancelled"
				}
				t.Run(phase, func(t *testing.T) {
					lock := c.repoLock(ready.Path)
					lock.Lock()
					parent, cancel := context.WithCancel(t.Context())
					ctx := &lockWaitContext{Context: parent, entered: make(chan struct{})}
					acquisitions := int32(1)
					if name == "receive" && !alreadyCancelled {
						// Receive first acquires its free transaction lock, then
						// attempts the repository lock held by this test.
						acquisitions = 2
					}
					ctx.acquisitions.Store(acquisitions)
					type result struct {
						err      error
						returned time.Time
					}
					completed := make(chan result, 1)
					finished := false
					t.Cleanup(func() {
						cancel()
						lock.Unlock()
						if !finished {
							select {
							case <-completed:
							case <-time.After(testtimeout.Start(250 * time.Millisecond)):
								t.Error("lock waiter did not stop after holder release")
							}
						}
					})
					if alreadyCancelled {
						cancel()
					}
					go func() {
						err := op(ctx)
						completed <- result{err: err, returned: time.Now()}
					}()
					select {
					case <-ctx.entered:
					case <-time.After(testtimeout.Start(250 * time.Millisecond)):
						t.Fatal("worker did not enter lock acquisition")
					}
					if !alreadyCancelled {
						select {
						case got := <-completed:
							finished = true
							t.Fatalf("uncancelled waiter completed while holder remained locked: %v", got.err)
						default:
						}
					}
					deadline := time.Now().Add(250 * time.Millisecond)
					cancel()
					var got result
					select {
					case got = <-completed:
					case <-time.After(250 * time.Millisecond):
						// A delayed observer may see both channels ready. The
						// worker's timestamp still enforces the original budget.
						select {
						case got = <-completed:
						default:
							t.Fatal("lock ignores cancellation")
						}
					}
					finished = true
					if got.returned.After(deadline) {
						t.Fatal("lock ignores cancellation")
					}
					if !errors.Is(got.err, context.Canceled) {
						t.Fatal(got.err)
					}
				})
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
