package main

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

func blockingParts(t *testing.T, names []string, closed *[]string, mu *sync.Mutex) []Part {
	t.Helper()
	var parts []Part
	for _, name := range names {
		entered, returned := make(chan struct{}), make(chan struct{})
		p := Part{Name: name, Run: func(ctx context.Context) error { close(entered); <-ctx.Done(); close(returned); return ctx.Err() }, Ready: func(context.Context) error { <-entered; return nil }, Close: func(context.Context) error {
			select {
			case <-returned:
			default:
				t.Error("close before Run joined")
			}
			mu.Lock()
			*closed = append(*closed, name)
			mu.Unlock()
			return nil
		}}
		parts = append(parts, p)
	}
	return parts
}

func TestPartsStartOnceStopReverse(t *testing.T) {
	var closed []string
	var mu sync.Mutex
	l := newLifecycle(context.Background(), blockingParts(t, []string{"store", "logs", "service"}, &closed, &mu))
	if err := l.start(); err != nil {
		t.Fatal(err)
	}
	if err := l.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := l.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(closed, []string{"service", "logs", "store"}) {
		t.Fatalf("close order %v", closed)
	}
}

func TestRecoverAfterWatchReady(t *testing.T) {
	ready := make(chan struct{})
	entered := make(chan struct{})
	var events []string
	var mu sync.Mutex
	record := func(s string) { mu.Lock(); events = append(events, s); mu.Unlock() }
	parts := []Part{{Name: "watch", Run: func(ctx context.Context) error { close(entered); <-ctx.Done(); return ctx.Err() }, Ready: func(context.Context) error { <-entered; <-ready; record("ready"); return nil }},
		{Name: "service", Recover: func(context.Context) error { record("recover"); return nil }}}
	l := newLifecycle(context.Background(), parts)
	done := make(chan error, 1)
	go func() { done <- l.start() }()
	<-entered
	select {
	case err := <-done:
		t.Fatalf("recovered before readiness: %v", err)
	default:
	}
	close(ready)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	record("ssh bind")
	if err := l.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"ready", "recover", "ssh bind"}) {
		t.Fatalf("order %v", events)
	}
}

func TestFailedStartStopsStartedParts(t *testing.T) {
	var closed []string
	var mu sync.Mutex
	parts := blockingParts(t, []string{"store", "watch"}, &closed, &mu)
	failed := errors.New("readiness refused")
	parts[1].Ready = func(context.Context) error { return failed }
	l := newLifecycle(context.Background(), parts)
	if err := l.start(); !errors.Is(err, failed) {
		t.Fatal("failed start accepted")
	}
	if err := l.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(closed, []string{"watch", "store"}) {
		t.Fatal(closed)
	}
}

func TestBackgroundReturnFailsServer(t *testing.T) {
	l := newLifecycle(t.Context(), []Part{{Name: "worker", Run: func(context.Context) error { return nil }}})
	_ = l.start()
	<-l.parts[0].done
	select {
	case err := <-l.failures:
		if err == nil {
			t.Fatal("worker exit accepted")
		}
	default:
		t.Fatal("worker exit not reported")
	}
	if err := l.stop(t.Context()); err == nil {
		t.Fatal("shutdown lost worker failure")
	}
}
