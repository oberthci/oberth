package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
)

// Covers: REQ-C11-021
func TestHonestResponsiveHealthContract(t *testing.T) {
	clock := fake.NewClock()
	valid := true
	h := &health{clock: serverClock{clock.Now, clock.Ticker}, validLeaf: func() bool { return valid }, sample: func(context.Context) error { return nil }}
	if h.ready(context.Background()) == nil {
		t.Fatal("ready before startup")
	}
	h.sampleOnce(context.Background())
	h.setStarted(true)
	if h.ready(context.Background()) != nil {
		t.Fatal("healthy server refused")
	}
	h.sample = func(context.Context) error { return errors.New("busy") }
	clock.Advance(89 * time.Second)
	h.sampleOnce(context.Background())
	if h.ready(context.Background()) != nil {
		t.Fatal("busy sample erased success")
	}
	clock.Advance(time.Second)
	if h.ready(context.Background()) == nil || h.live(context.Background()) != nil {
		t.Fatal("stuck store health is dishonest")
	}
	clock.Advance(5*time.Minute - time.Second)
	if h.live(context.Background()) == nil {
		t.Fatal("wedged sampler is live")
	}
	h.sample = func(context.Context) error { return nil }
	h.sampleOnce(context.Background())
	valid = false
	if h.ready(context.Background()) == nil {
		t.Fatal("invalid leaf is ready")
	}
	assertStartupGate(t, h)
}

func assertStartupGate(t *testing.T, h *health) {
	t.Helper()
	h.setStarted(false)
	handler := h.gate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	for _, path := range []string{"/mcp", "/api/runs", "/livez", "/readyz"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		want := 503
		if path == "/livez" {
			want = 200
		}
		if w.Code != want {
			t.Fatalf("startup %s: %d", path, w.Code)
		}
	}
}

func TestEventRetentionTicker(t *testing.T) {
	clock := fake.NewClock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := make(chan [2]time.Time, 1)
	done := make(chan error, 1)
	go func() {
		h := &health{clock: serverClock{clock.Now, clock.Ticker}, readySignal: make(chan struct{})}
		h.setStarted(true)
		done <- h.retainEvents(ctx, func(_ context.Context, ordinary, progress time.Time) error {
			calls <- [2]time.Time{ordinary, progress}
			return nil
		}, io.Discard)
	}()
	if interval := <-clock.Started; interval != 5*time.Minute {
		t.Fatal("retention interval", interval)
	}
	clock.Advance(5 * time.Minute)
	cutoff := <-calls
	if cutoff[0] != clock.Now().Add(-30*24*time.Hour) || cutoff[1] != clock.Now().Add(-24*time.Hour) {
		t.Fatal("wrong cutoffs")
	}
	cancel()
	<-done
	if clock.ActiveTickers() != 0 {
		t.Fatal("retention ticker leaked")
	}
}

func TestHealthSamplerCancelsAndJoins(t *testing.T) {
	clock := fake.NewClock()
	entered := make(chan struct{})
	done := make(chan error, 1)
	h := &health{clock: serverClock{clock.Now, clock.Ticker}, sample: func(ctx context.Context) error { close(entered); <-ctx.Done(); return ctx.Err() }}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { done <- h.run(ctx) }()
	<-entered
	cancel()
	<-done
	if clock.ActiveTickers() != 0 {
		t.Fatal("sampler ticker leaked")
	}
}
