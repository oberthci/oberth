package githubfake

import (
	"context"
	"sync"
	"testing"
	"time"
)

type delayGate struct {
	entered chan time.Duration
	release chan struct{}
	once    sync.Once
}

func newDelayGate() *delayGate {
	return &delayGate{entered: make(chan time.Duration, 1), release: make(chan struct{})}
}
func (g *delayGate) wait(ctx context.Context, duration time.Duration) bool {
	select {
	case g.entered <- duration:
	case <-ctx.Done():
		return false
	}
	select {
	case <-g.release:
		return true
	case <-ctx.Done():
		return false
	}
}
func (g *delayGate) open() { g.once.Do(func() { close(g.release) }) }
func (g *delayGate) await(t *testing.T, want time.Duration) {
	t.Helper()
	select {
	case duration := <-g.entered:
		if duration != want {
			t.Fatalf("delay %s, want %s", duration, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request did not enter delay")
	}
}
func TestDelayCompletionAndCancellation(t *testing.T) {
	if !delay(context.Background(), 0) {
		t.Fatal("zero delay did not complete")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if delay(ctx, time.Hour) {
		t.Fatal("cancelled delay completed")
	}
}
