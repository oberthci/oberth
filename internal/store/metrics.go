package store

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// latency retains bounded cumulative buckets; no actor or repository labels.
type latency struct {
	mu      sync.Mutex
	buckets [8]uint64
	count   uint64
	seconds float64
}

func latencyBounds() [8]time.Duration {
	return [8]time.Duration{time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond, 25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, time.Second, 5 * time.Second}
}

func (h *latency) record(elapsed time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	elapsed = max(0, elapsed)
	h.count++
	h.seconds += elapsed.Seconds()
	for i, bound := range latencyBounds() {
		if elapsed <= bound {
			h.buckets[i]++
		}
	}
}

func (h *latency) render(out *strings.Builder, name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintf(out, "# TYPE %s histogram\n", name)
	for i, bound := range latencyBounds() {
		fmt.Fprintf(out, "%s_bucket{le=\"%g\"} %d\n", name, bound.Seconds(), h.buckets[i])
	}
	fmt.Fprintf(out, "%s_bucket{le=\"+Inf\"} %d\n%s_count %d\n%s_sum %g\n", name, h.count, name, h.count, name, h.seconds)
}

type storeMetrics struct{ slot, lock, transaction, events, fanout latency }

// Metrics is a read-only process baseline for the existing operator endpoint.
// Fanout measures committed-change channel publication, not client notice time.
func (s *Store) Metrics() string {
	var out strings.Builder
	for _, m := range []struct {
		name  string
		value *latency
	}{
		{"writer_slot", &s.metrics.slot}, {"sqlite_lock", &s.metrics.lock},
		{"transaction", &s.metrics.transaction}, {"event_query", &s.metrics.events},
		{"change_fanout", &s.metrics.fanout},
	} {
		m.value.render(&out, "oberth_"+m.name+"_seconds")
	}
	stats := s.readers.Stats()
	fmt.Fprintf(&out, "oberth_reader_wait_total %d\noberth_reader_wait_seconds_total %g\n", stats.WaitCount, stats.WaitDuration.Seconds())
	return out.String()
}
