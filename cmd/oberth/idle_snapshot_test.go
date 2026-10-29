package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestIdleSnapshotAdminProbeDoesNotWaitForStorage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		snapshot := &idleQueueSnapshot{}
		calls := 0
		release := make(chan struct{})
		joined := make(chan struct{})
		go func() {
			defer close(joined)
			snapshot.watch(ctx, func(ctx context.Context) (model.IdleQueueStatus, error) {
				calls++
				if calls == 1 {
					return model.IdleQueueStatus{Version: 1, Queued: 1, Tag: 1}, nil
				}
				if calls == 2 {
					select {
					case <-release:
						return model.IdleQueueStatus{Version: 1}, nil
					case <-ctx.Done():
						return model.IdleQueueStatus{}, ctx.Err()
					}
				}
				return model.IdleQueueStatus{}, errors.New("database unavailable")
			})
		}()
		synctest.Wait()
		server := newAdminGateServer(nil, "fixture", nil, snapshot.current)
		probe := func(want int) {
			t.Helper()
			start := time.Now()
			response := httptest.NewRecorder()
			server.Handler.ServeHTTP(response, httptest.NewRequest("GET", "/v1/idle?wait_for=critical", nil))
			if response.Code != want || time.Since(start) != 0 {
				t.Fatalf("status=%d want=%d elapsed=%s", response.Code, want, time.Since(start))
			}
		}
		probe(http.StatusConflict)
		time.Sleep(time.Second)
		synctest.Wait()
		for range 100 {
			probe(http.StatusConflict)
		}
		if calls != 2 {
			t.Fatalf("admin requests read storage: %d reads", calls)
		}
		time.Sleep(4 * time.Second)
		probe(http.StatusServiceUnavailable) // cached sample expires during a blocked read
		close(release)
		synctest.Wait()
		probe(http.StatusNoContent)
		time.Sleep(time.Second)
		synctest.Wait()
		probe(http.StatusServiceUnavailable) // failed refresh invalidates even a recent idle sample
		cancel()
		<-joined
	})
}

func TestIdleSnapshotRejectsMissingInvalidAndSlowSamples(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		snapshot := &idleQueueSnapshot{}
		if _, err := snapshot.current(t.Context()); err == nil {
			t.Fatal("missing sample accepted")
		}
		ctx, cancel := context.WithCancel(t.Context())
		joined := make(chan struct{})
		calls := 0
		go func() {
			defer close(joined)
			snapshot.watch(ctx, func(ctx context.Context) (model.IdleQueueStatus, error) {
				calls++
				if calls == 1 {
					return model.IdleQueueStatus{Version: 2}, nil
				}
				<-ctx.Done()
				return model.IdleQueueStatus{Version: 1}, nil // late success must stay unavailable
			})
		}()
		synctest.Wait()
		if _, err := snapshot.current(t.Context()); err == nil {
			t.Fatal("invalid sample accepted")
		}
		time.Sleep(6 * time.Second)
		synctest.Wait()
		if _, err := snapshot.current(t.Context()); err == nil {
			t.Fatal("timed out sample accepted")
		}
		cancel()
		<-joined
	})
}
