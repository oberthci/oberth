package gitcache_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestReceiveRecordWaitsForSavedRunsAcknowledgement(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	f := gittest.New(t)
	f.Write(t, "file.txt", []byte("admitted source\n"))
	next := f.CommitFile(t, "push")
	c := receiveCache(t, f, func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { return nil })
	updates := []gitcache.RefUpdate{{Ref: "refs/heads/feature", NewSHA: next}}
	r := gateReceive(f)
	r.AlwaysRun = true
	r.Input = receiveBatchInput(t, f, updates, nil)
	entered := make(chan gitcache.ReceiveEvent, 1)
	finishSave := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(finishSave) }) }
	r.Deliver = func(ctx context.Context, e gitcache.ReceiveEvent) ([]string, error) {
		entered <- e
		select {
		case <-finishSave:
			return nil, io.ErrClosedPipe
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Receive(ctx, r); close(done) }()
	defer func() { release(); <-done }()
	event := awaitReceiveDelivery(ctx, t, entered, done)
	assertReadyReceive(t.Context(), t, c, event)
	release()
	if err := <-done; !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	assertReadyReceive(t.Context(), t, c, event)
	// A new cache represents the server after losing its in-memory callbacks.
	_ = c.Close()
	restarted := receiveCache(t, f, nil)
	saved := false
	if err := restarted.ReplayPending(t.Context(), func(ctx context.Context, e gitcache.ReceiveEvent) ([]string, error) {
		assertReadyReceive(ctx, t, restarted, e)
		assertAttribution(t, e, event)
		saved = true
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if !saved {
		t.Fatal("runs were not saved before acknowledgement")
	}
	if _, err := restarted.PendingReceive(t.Context(), gittest.Repository); !os.IsNotExist(err) {
		t.Fatal("acknowledged record retained", err)
	}
}

func awaitReceiveDelivery(ctx context.Context, t *testing.T, entered <-chan gitcache.ReceiveEvent, done <-chan error) gitcache.ReceiveEvent {
	t.Helper()
	select {
	case event := <-entered:
		return event
	case err := <-done:
		t.Fatal("receive ended before delivery", err)
	case <-ctx.Done():
		t.Fatal("receive did not reach delivery", context.Cause(ctx))
	}
	return gitcache.ReceiveEvent{}
}

func assertReadyReceive(ctx context.Context, t *testing.T, c *gitcache.Cache, e gitcache.ReceiveEvent) {
	t.Helper()
	r, err := c.PendingReceive(ctx, gittest.Repository)
	if err != nil || r.State != "ready" || r.Event.ID != e.ID {
		t.Fatalf("unacknowledged record lost: %+v %v", r, err)
	}
}

func TestRetryDeliveryDoesNotRewriteReceiveRecord(t *testing.T) {
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { return nil }
	c := thirdInit(t, cfg)
	record, _ := pendingAbandonReceive(t, c, f)
	path := filepath.Join(f.Store(), "oberth-receive.json")
	original, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(f.Store(), 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.Store(), 0700) })
	calls := 0
	for range 3 {
		err = c.RetryReceive(t.Context(), gittest.Repository, func(_ context.Context, event gitcache.ReceiveEvent) ([]string, error) {
			calls++
			if event.ID != record.Event.ID {
				t.Error("delivery changed identity", event.ID)
			}
			return nil, io.ErrClosedPipe
		})
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatal("retry required a record write before delivery", err)
		}
	}
	current, err := os.Stat(path)
	if err != nil || !os.SameFile(original, current) || calls != 3 {
		t.Fatal("retry rewrote its durable evidence", calls, err)
	}
	if err = os.Chmod(f.Store(), 0700); err != nil {
		t.Fatal(err)
	}
	retryCompletion(t, c, func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil })
	if completionState(t, f) != "absent" {
		t.Fatal("acknowledged retry did not settle")
	}
}
