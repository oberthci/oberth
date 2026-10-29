package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/testtimeout"
)

func TestHTTPHandoverBoundsStalledRequests(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(stopped)
	}))
	defer server.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		response, err := server.Client().Get(server.URL)
		if err == nil {
			response.Body.Close()
		}
	}()
	<-started
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := shutdownHTTP(ctx, server.Config); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error=%v", err)
	}
	select {
	case <-stopped:
	case <-time.After(testtimeout.Start(time.Second)):
		t.Fatal("stalled request survived drain bound")
	}
	select {
	case <-done:
	case <-time.After(testtimeout.Start(time.Second)):
		t.Fatal("client connection survived drain bound")
	}
}
