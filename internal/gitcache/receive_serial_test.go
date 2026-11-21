package gitcache_test

import (
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

type waitingReceive struct {
	ready chan struct{}
	once  sync.Once
}

func (w *waitingReceive) Write(data []byte) (int, error) {
	if strings.Contains(string(data), "waiting") {
		w.once.Do(func() { close(w.ready) })
	}
	return len(data), nil
}

func TestReceiveCompletionSerializesNextPush(t *testing.T) {
	for _, mode := range []string{"success", "disconnect", "missing-disconnect"} {
		t.Run(mode, func(t *testing.T) { checkReceiveSerialization(t, mode) })
	}
}

func checkReceiveSerialization(t *testing.T, mode string) {
	t.Helper()
	f, c, next := completionFixture(t)
	f.Git(t, f.Store(), "update-ref", "refs/heads/second", f.Commit)
	first, second := completionRequest(t, f, next), completionRequest(t, f, next)
	second.Input = receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/second", OldSHA: f.Commit, NewSHA: next})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	status := &completionStatus{ctx: ctx, ready: make(chan struct{}), release: make(chan struct{})}
	if mode != "success" {
		status.failure = io.ErrClosedPipe
	}
	var release sync.Once
	resume := func() { release.Do(func() { close(status.release) }) }
	defer resume()
	first.Output = status
	var damage sync.Once
	if mode == "missing-disconnect" {
		first.Begin = func() { damage.Do(func() { damageCompletionHook(t, f, "missing") }) }
	}
	waiting := &waitingReceive{ready: make(chan struct{})}
	second.Diagnostics = waiting
	var delivered atomic.Int32
	deliver := func(context.Context, gitcache.ReceiveEvent) ([]string, error) { delivered.Add(1); return nil, nil }
	first.Deliver, second.Deliver = deliver, deliver
	one, two := make(chan error, 1), make(chan error, 1)
	go func() { one <- c.Receive(ctx, first) }()
	select {
	case <-status.ready:
	case err := <-one:
		t.Fatal("first receive ended before final status", err)
	case <-ctx.Done():
		t.Fatal("first receive did not reach final status")
	}
	go func() { two <- c.Receive(ctx, second) }()
	select {
	case <-waiting.ready:
	case err := <-two:
		t.Fatal("second receive did not wait", err)
	case <-ctx.Done():
		t.Fatal("second receive did not report waiting")
	}
	resume()
	awaitSerializedReceives(ctx, t, []chan error{one, two}, mode)
	retryCompletion(t, c, deliver)
	assertSerializedReceives(t, f, mode, serializedReceiveResult{next: next, delivered: delivered.Load()})
}

func awaitSerializedReceives(ctx context.Context, t *testing.T, results []chan error, mode string) {
	t.Helper()
	for i, done := range results {
		select {
		case err := <-done:
			if (err == nil) != (i == 1 || mode == "success") {
				t.Error("receive result", i, err)
			}
		case <-ctx.Done():
			t.Fatal("serialized receive did not finish", i)
		}
	}
}

type serializedReceiveResult struct {
	next      string
	delivered int32
}

func assertSerializedReceives(t *testing.T, f *gittest.Fixture, mode string, result serializedReceiveResult) {
	t.Helper()
	wantRef, wantCount := result.next, int32(2)
	if mode == "missing-disconnect" {
		wantRef, wantCount = f.Commit, 1
	}
	if got := f.Git(t, f.Store(), "rev-parse", "refs/heads/topic"); got != wantRef {
		t.Error("first ref", got, wantRef)
	}
	if got := f.Git(t, f.Store(), "rev-parse", "refs/heads/second"); got != result.next {
		t.Error("second ref", got, result.next)
	}
	if result.delivered != wantCount {
		t.Error("delivery count", result.delivered, wantCount)
	}
}
