package goproxy

import (
	"bytes"
	"fmt"
	"log"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestProxyFailureLogsCoalesceConcurrentRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var output bytes.Buffer
		handler := routingFixture("").WithLogger(log.New(&output, "", 0))
		request := func(method, path string, status int) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(method, path, nil))
			if response.Code != status {
				t.Errorf("rate limiter changed response to %d, want %d", response.Code, status)
			}
		}
		const requests = 1000
		var workers sync.WaitGroup
		for index := range requests {
			workers.Go(func() {
				// Rotating paths must share the private_unavailable class window.
				request("GET", fmt.Sprintf("/go.example.test/unknown%d/@v/list", index), 403)
			})
		}
		workers.Wait()
		if count := strings.Count(output.String(), "goproxy request failed:"); count != 1 {
			t.Fatalf("failure burst emitted %d lines, want 1", count)
		}
		request("POST", "/", 405) // Another class gets its own first log.
		if strings.Count(output.String(), "goproxy request failed:") != 2 {
			t.Fatal("private failures suppressed a different failure class")
		}
		time.Sleep(failureLogInterval - time.Nanosecond)
		request("GET", "/go.example.test/unknown/@v/list", 403)
		if strings.Count(output.String(), "class=private_unavailable") != 1 {
			t.Fatal("emitted another log before the interval expired")
		}
		time.Sleep(time.Nanosecond)
		request("GET", "/go.example.test/unknown/@v/list", 403)
		if strings.Count(output.String(), "class=private_unavailable") != 2 || !strings.Contains(output.String(), "suppressed=1000\n") {
			t.Fatalf("coalesced count missing at interval boundary: %s", &output)
		}
		request("GET", "/go.example.test/unknown/@v/list", 403)
		time.Sleep(failureLogInterval)
		request("GET", "/go.example.test/unknown/@v/list", 403)
		if !strings.Contains(output.String(), "suppressed=1\n") {
			t.Fatalf("suppression counter did not reset: %s", &output)
		}
	})
}

func TestProxyFailureLogClassesHaveBoundedWindows(t *testing.T) {
	var limiter failureLogLimiter
	now := time.Now()
	for _, class := range []string{
		"method_not_allowed", "private_unavailable", "private_backend_unavailable",
		"internal_error", "archive_error", "zip_creation_error", "response_write_failed",
	} {
		if count, allowed := limiter.allow(class, now); !allowed || count != 0 {
			t.Fatalf("class %q did not get an independent initial window", class)
		}
		if _, allowed := limiter.allow(class, now); allowed {
			t.Fatalf("class %q emitted twice in one interval", class)
		}
	}
	if _, allowed := limiter.allow("future_class", now); !allowed {
		t.Fatal("fallback window was consumed by a known class")
	}
	for index := range 1000 {
		if _, allowed := limiter.allow(fmt.Sprintf("unknown%d", index), now); allowed {
			t.Fatal("unknown class names created independent windows")
		}
	}
	if count, allowed := limiter.allow("future_class", now.Add(failureLogInterval)); !allowed || count != 1000 {
		t.Fatalf("fallback counter: allowed=%v suppressed=%d", allowed, count)
	}
}
