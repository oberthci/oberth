package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestSampledButNotStartedRemainsUnready(t *testing.T) {
	clock := fake.NewClock()
	h := &health{clock: serverClock{clock.Now, clock.Ticker}, sample: func(context.Context) error { return nil }, validLeaf: func() bool { return true }}
	if !strings.Contains(h.metrics(t.Context()), "sample_age_seconds -1\n") {
		t.Fatal("unsampled age is not unknown")
	}
	h.sampleOnce(t.Context())
	if err := h.ready(t.Context()); err == nil || !strings.Contains(err.Error(), "starting") {
		t.Fatal("ready before started")
	}
	h.setStarted(true)
	if !strings.Contains(h.metrics(t.Context()), "oberth_started 1\n") || strings.Contains(h.metrics(t.Context()), "watch_synced") {
		t.Fatal("metric misrepresents watch state")
	}
}
func TestSamplerBudgetAndFiveSecondInterval(t *testing.T) {
	clock := fake.NewClock()
	parent, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	end, _ := parent.Deadline()
	h := &health{clock: serverClock{clock.Now, clock.Ticker}, sample: func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok || !deadline.Before(end.Add(-29*time.Second)) {
			t.Error("sample has no 30 second budget")
		}
		return nil
	}}
	h.sampleOnce(parent)
	calls := make(chan struct{}, 2)
	h.sample = func(context.Context) error { calls <- struct{}{}; return nil }
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()
	if interval := <-clock.Started; interval != 5*time.Second {
		t.Fatal("wrong sample interval")
	}
	<-calls
	clock.Advance(5 * time.Second)
	<-calls
	stop()
	<-done
}
func TestRetentionFailureIsLoggedAndRetriedDaily(t *testing.T) {
	clock := fake.NewClock()
	h := &health{clock: serverClock{clock.Now, clock.Ticker}, readySignal: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := make(chan [2]time.Time, 2)
	out := &serverTrace{ready: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- h.retainEvents(ctx, func(_ context.Context, a, b time.Time) error {
			calls <- [2]time.Time{a, b}
			return errors.New("storage busy")
		}, out)
	}()
	h.setStarted(true)
	if d := <-clock.Started; d != 5*time.Minute {
		t.Fatal("first retention not at five minutes")
	}
	clock.Advance(5 * time.Minute)
	<-calls
	select {
	case d := <-clock.Started:
		if d != 24*time.Hour {
			t.Fatal("retention not daily")
		}
	case err := <-done:
		t.Fatalf("retention failure killed server: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("retention failure killed server: %v", err)
	default:
	}
	if !strings.Contains(out.String(), "Event retention failed.") || !strings.Contains(h.metrics(t.Context()), "retention_failures_total 1\n") {
		t.Fatal("retention failure not observed")
	}
	clock.Advance(24 * time.Hour)
	cutoff := <-calls
	if cutoff[0] != clock.Now().Add(-30*24*time.Hour) || cutoff[1] != clock.Now().Add(-24*time.Hour) {
		t.Fatal("wrong retention cutoffs")
	}
	cancel()
	<-done
}
func TestRetentionDoesNotArmBeforeReady(t *testing.T) {
	clock := fake.NewClock()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	h := &health{clock: serverClock{clock.Now, clock.Ticker}, readySignal: make(chan struct{})}
	if err := h.retainEvents(ctx, func(context.Context, time.Time, time.Time) error { t.Error("retained before ready"); return nil }, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-clock.Started:
		t.Fatal("retention ticker started before readiness")
	default:
	}
}
func TestCacheMaintenanceStartsHourlyAndStops(t *testing.T) {
	f := fixtureServer(t)
	called := make(chan struct{}, 1)
	original := f.deps.constructors.CacheNew
	f.deps.constructors.CacheNew = func(ctx context.Context, c *composition) (Part, error) {
		p, err := original(ctx, c)
		c.cacheGC = func(context.Context) error { called <- struct{}{}; return nil }
		return p, err
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := f.start(ctx)
	intervals := map[time.Duration]bool{}
	for len(intervals) < 3 {
		intervals[<-f.clock.Started] = true
	}
	if !intervals[time.Hour] {
		t.Fatal("cache maintenance not hourly")
	}
	f.clock.Advance(time.Hour)
	<-called
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if f.clock.ActiveTickers() != 0 {
		t.Fatal("cache ticker leaked")
	}
}

func TestHealthReasonsRemainSpecificAndWithinFortyCharacters(t *testing.T) {
	for _, kind := range []string{"starting", "unsampled", "stale", "certificate", "stopping", "stalled", "maximum age"} {
		t.Run(kind, func(t *testing.T) { healthReasonCase(t, kind) })
	}
}
func healthReasonCase(t *testing.T, kind string) {
	t.Helper()
	clock := fake.NewClock()
	valid := true
	h := &health{clock: serverClock{clock.Now, clock.Ticker}, validLeaf: func() bool { return valid }, success: clock.Now(), completed: clock.Now(), started: true}
	switch kind {
	case "starting":
		h.started = false
	case "unsampled":
		h.success = time.Time{}
	case "stale":
		clock.Advance(95 * time.Second)
	case "certificate":
		valid = false
	case "stopping":
		h.stop()
	case "stalled":
		clock.Advance(5 * time.Minute)
	case "maximum age":
		clock.Advance(time.Duration(1<<63 - 1))
	}
	err := h.ready(t.Context())
	if kind == "stalled" {
		err = h.live(t.Context())
	}
	var public *publicerr.Error
	if !errors.As(err, &public) || publicerr.CheckMessage(public.Reason(), 40) != nil || public.Reason() == "Server is not ready." {
		t.Fatalf("unhelpful health reason: %v", err)
	}
}
