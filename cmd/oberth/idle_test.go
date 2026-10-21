package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestWaitIdleObservesWholeLiveQueue(t *testing.T) {
	var calls atomic.Int32
	admin := newAdminGateServer(nil, "unused", nil, func(context.Context) (model.RunCounts, error) {
		switch calls.Add(1) {
		case 1:
			return model.RunCounts{Queued: 1}, nil
		case 2:
			return model.RunCounts{Running: 1}, nil
		default:
			return model.RunCounts{}, nil
		}
	})
	server := httptest.NewServer(admin.Handler)
	defer server.Close()
	if err := waitIdle(t.Context(), server.Client(), server.URL+"/v1/idle", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("ignored nonterminal runs: %d probes", calls.Load())
	}
}

func TestWaitIdleFailsClosedAndIsBounded(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusServiceUnavailable, http.StatusNotFound} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
		err := waitIdle(ctx, server.Client(), server.URL, time.Millisecond)
		cancel()
		server.Close()
		if err == nil {
			t.Fatalf("status %d accepted", status)
		}
		if status == http.StatusConflict && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	}
}
