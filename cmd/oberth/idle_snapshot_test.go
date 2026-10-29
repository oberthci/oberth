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
		probe(http.StatusConflict) // cached sample stays fresh while the next read is blocked
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

func TestIdleSnapshotSlowReadsRemainAvailableToWaiter(t *testing.T) {
	for _, phase := range []time.Duration{0, 5 * time.Second, 7 * time.Second, 9500 * time.Millisecond, 12 * time.Second, 30 * time.Second} {
		t.Run(phase.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				snapshot := &idleQueueSnapshot{}
				joined := make(chan struct{})
				go func() {
					defer close(joined)
					snapshot.watch(ctx, func(ctx context.Context) (model.IdleQueueStatus, error) {
						timer := time.NewTimer(4500 * time.Millisecond)
						defer timer.Stop()
						select {
						case <-ctx.Done():
							return model.IdleQueueStatus{}, ctx.Err()
						case <-timer.C:
							return model.IdleQueueStatus{Version: 1}, nil
						}
					})
				}()
				synctest.Wait()
				time.Sleep(phase)
				admin := newAdminGateServer(nil, "fixture", nil, snapshot.current)
				client := &http.Client{Transport: idleRoundTrip(func(r *http.Request) (*http.Response, error) {
					response := httptest.NewRecorder()
					admin.Handler.ServeHTTP(response, r)
					return response.Result(), nil
				})}
				start := time.Now()
				waitCtx, stopWait := context.WithTimeout(ctx, 10*time.Second)
				defer stopWait()
				if err := waitIdle(waitCtx, client, "http://fixture/v1/idle?wait_for=critical", time.Second, idleWaitOptions{mode: "critical"}); err != nil || time.Since(start) > 7*time.Second {
					t.Fatalf("phase=%s elapsed=%s err=%v", phase, time.Since(start), err)
				}
				cancel()
				<-joined
			})
		})
	}
}

func TestIdleSnapshotExpiresDuringStuckRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		snapshot := &idleQueueSnapshot{}
		release := make(chan struct{})
		joined := make(chan struct{})
		calls := 0
		go func() {
			defer close(joined)
			snapshot.watch(ctx, func(context.Context) (model.IdleQueueStatus, error) {
				calls++
				if calls > 1 {
					<-release // model a reader that cannot promptly honor its deadline
				}
				return model.IdleQueueStatus{Version: 1}, nil
			})
		}()
		synctest.Wait()
		time.Sleep(idleSnapshotMaxAge)
		if _, err := snapshot.current(t.Context()); err == nil {
			t.Fatal("expired idle sample accepted during stuck read")
		}
		close(release)
		synctest.Wait()
		if _, err := snapshot.current(t.Context()); err == nil {
			t.Fatal("late idle result accepted after its read deadline")
		}
		cancel()
		<-joined
	})
}
