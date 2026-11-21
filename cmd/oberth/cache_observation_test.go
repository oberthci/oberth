package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
)

type noticeWriter struct{ wrote chan string }

func (w noticeWriter) Write(data []byte) (int, error) { w.wrote <- string(data); return len(data), nil }

func TestCacheObservationsDoNotBlockAndReachMaintenance(t *testing.T) {
	clock := fake.NewClock()
	out := noticeWriter{make(chan string, 1)}
	c := &composition{clock: serverClock{clock.Now, clock.Ticker}, health: &health{}, diagnostic: out, cacheNotices: make(chan cacheNotice, 1), cacheGC: func(context.Context) error { return nil }}
	owner := serverOwners{c: c}
	owner.cacheObservation("recovery set-aside", "origin/team/widget")
	returned := make(chan struct{})
	go func() { owner.cacheObservation("full queue", "other"); close(returned) }()
	select {
	case <-returned:
	case <-time.After(time.Second):
		<-c.cacheNotices
		<-returned
		t.Fatal("cache observation blocked the admission callback")
	}
	if c.health.cacheNoticeDrops.Load() != 1 {
		t.Fatal("observation loss was not counted", c.health.cacheNoticeDrops.Load())
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.maintainCache(ctx) }()
	select {
	case line := <-out.wrote:
		if !strings.Contains(line, "recovery set-aside") || !strings.Contains(line, "detail=origin/team/widget") || strings.Contains(line, "resource=") {
			t.Fatal("notice lost its diagnostic context", line)
		}
	case <-time.After(time.Second):
		t.Fatal("maintenance did not publish the queued cache notice")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cache maintenance did not stop")
	}
}
