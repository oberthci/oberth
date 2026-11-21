package main

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestCompositionR1UndrainedRequestKeepsDependencies(t *testing.T) {
	f := compositionRegressionFixture(t)
	entered, release, handlerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	f.source = http.HandlerFunc(func(http.ResponseWriter, *http.Request) { close(entered); <-release; close(handlerDone) })
	drainCancel := make(chan context.CancelFunc, 1)
	calls := 0
	f.deps.timeout = func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		calls++
		if calls == 2 {
			ctx, cancel := context.WithCancel(parent)
			drainCancel <- cancel
			return ctx, cancel
		}
		return context.WithTimeout(parent, d)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := f.start(ctx)
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		response, err := f.client(f.config.Listen.Source).Get("https://oberth.local/pending")
		if err == nil {
			_ = response.Body.Close()
		}
	}()
	<-entered
	cancel()
	expire := <-drainCancel
	expire()
	failure := <-done
	storage := f.c.store.Readiness(t.Context())
	events := f.trace.snapshot()
	listener := f.listener(f.config.Listen.SSH)
	select {
	case <-listener.closed:
	default:
		t.Error("SSH listener left open")
	}
	close(release)
	<-handlerDone
	<-requestDone
	if err := f.c.lifecycle.stop(t.Context()); err != nil {
		t.Log("cleanup", err)
	}
	if failure == nil {
		t.Error("HTTP drain failure exited successfully")
	}
	for _, event := range events {
		if event == "close service" {
			t.Error("service closed beneath HTTP handler")
		}
	}
	if storage != nil {
		t.Fatal("store closed beneath undrained HTTP request", storage)
	}
}
