package goproxy

import (
	"sync"
	"time"
)

const failureLogInterval = 30 * time.Second

type failureLogWindow struct {
	until      time.Time
	suppressed uint64
}

// A fixed set of windows bounds both log volume and limiter memory. Request
// paths and other untrusted values never become bucket keys. New error classes
// share the fallback window until explicitly assigned one here.
type failureLogLimiter struct {
	mu      sync.Mutex
	windows [8]failureLogWindow
}

func (limiter *failureLogLimiter) allow(class string, now time.Time) (uint64, bool) {
	index := 7
	switch class {
	case "method_not_allowed":
		index = 0
	case "private_unavailable":
		index = 1
	case "private_backend_unavailable":
		index = 2
	case "internal_error":
		index = 3
	case "archive_error":
		index = 4
	case "zip_creation_error":
		index = 5
	case "response_write_failed":
		index = 6
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	window := &limiter.windows[index]
	if now.Before(window.until) {
		if window.suppressed != ^uint64(0) {
			window.suppressed++
		}
		return 0, false
	}
	suppressed := window.suppressed
	window.until, window.suppressed = now.Add(failureLogInterval), 0
	return suppressed, true
}
