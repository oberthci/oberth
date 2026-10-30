package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestWaitIdleObservesWholeLiveQueue(t *testing.T) {
	var calls atomic.Int32
	admin := newAdminGateServer(nil, "unused", nil, func(context.Context) (model.IdleQueueStatus, error) {
		switch calls.Add(1) {
		case 1:
			return model.IdleQueueStatus{Version: 1, Queued: 1, Branch: 1}, nil
		case 2:
			return model.IdleQueueStatus{Version: 1, Running: 1, Tag: 1}, nil
		default:
			return model.IdleQueueStatus{Version: 1}, nil
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
		if (status == http.StatusConflict || status == http.StatusServiceUnavailable) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	}
}

type idleRoundTrip func(*http.Request) (*http.Response, error)

func (f idleRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWaitIdleBusyPastTwoMinutesWithFakeClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		calls := 0
		client := &http.Client{Transport: idleRoundTrip(func(*http.Request) (*http.Response, error) {
			calls++
			status := http.StatusConflict
			if time.Since(start) >= 3*time.Minute {
				status = http.StatusNoContent
			}
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"version":1,"queued":2,"running":1,"branch":1,"tag":1,"promotion":1}`))}, nil
		})}
		var output bytes.Buffer
		ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
		defer cancel()
		if err := waitIdle(ctx, client, "http://fixture/v1/idle", time.Second, idleWaitOptions{mode: "all", output: &output}); err != nil {
			t.Fatal(err)
		}
		if time.Since(start) != 3*time.Minute || calls != 181 || !strings.Contains(output.String(), "queued=2 running=1 branch=1 critical=2") || !strings.Contains(output.String(), "elapsed=2m0s") {
			t.Fatalf("calls=%d elapsed=%s output=%s", calls, time.Since(start), &output)
		}
	})
}

func TestWaitIdleCriticalRequiresSnapshotCapability(t *testing.T) {
	for _, test := range []struct {
		name  string
		state model.IdleQueueStatus
		mode  string
		code  int
	}{
		{"branch only all", model.IdleQueueStatus{Version: 1, Queued: 1, Branch: 1}, "all", 409},
		{"branch only critical", model.IdleQueueStatus{Version: 1, Queued: 1, Branch: 1}, "critical", 204},
		{"promotion", model.IdleQueueStatus{Version: 1, Running: 1, Promotion: 1}, "critical", 409},
		{"apply", model.IdleQueueStatus{Version: 1, Queued: 1, Apply: 1}, "critical", 409},
		{"invalid", model.IdleQueueStatus{Version: 1, Queued: 1}, "all", 503},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newAdminGateServer(nil, "unused", nil, func(context.Context) (model.IdleQueueStatus, error) { return test.state, nil })
			request := httptest.NewRequest("GET", "/v1/idle?wait_for="+test.mode, nil)
			response := httptest.NewRecorder()
			server.Handler.ServeHTTP(response, request)
			if response.Code != test.code {
				t.Fatalf("%d %s", response.Code, response.Body.String())
			}
		})
	}
	client := &http.Client{Transport: idleRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 204, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	if err := waitIdle(t.Context(), client, "http://old/v1/idle", time.Second, idleWaitOptions{mode: "critical"}); err == nil || !strings.Contains(err.Error(), "does not support critical") {
		t.Fatal(err)
	}
}

func TestWaitIdleCriticalReportsStatusBeforeCapability(t *testing.T) {
	for _, status := range []int{400, 503, 204, 409} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			client := &http.Client{Transport: idleRoundTrip(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
			})}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
			defer cancel()
			err := waitIdle(ctx, client, "http://fixture/v1/idle", time.Second, idleWaitOptions{mode: "critical"})
			want := "does not support critical queue observation"
			if status == 400 || status == 503 {
				want = fmt.Sprintf("live daemon queue unavailable (HTTP %d)", status)
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("status %d: %v; want %q", status, err, want)
			}
		})
	}
}
