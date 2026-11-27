package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/stateport"
)

func TestBaselineDiagnostics(t *testing.T) {
	s := testStore(t)
	tx, done, err := s.beginWrite(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback()
	done()
	ctx, cancel := context.WithCancel(t.Context())
	s.writes <- struct{}{}
	cancel()
	_, _, err = s.beginWrite(ctx)
	<-s.writes
	if err == nil {
		t.Fatal("blocked writer ignored cancellation")
	}
	_, err = s.Events(t.Context(), stateport.EventFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	changed := s.Changes()
	s.signalChange()
	select {
	case <-changed:
	default:
		t.Fatal("publication did not wake observer")
	}
	metrics := s.Metrics()
	for _, line := range []string{"oberth_writer_slot_seconds_count 2\n", "oberth_sqlite_lock_seconds_count 1\n", "oberth_transaction_seconds_count 1\n", "oberth_event_query_seconds_count 1\n", "oberth_change_fanout_seconds_count 1\n"} {
		if !strings.Contains(metrics, line) {
			t.Fatalf("missing %q in %s", line, metrics)
		}
	}
	if metrics != s.Metrics() {
		t.Fatal("metrics read mutated baseline")
	}
}

func TestLatencyBuckets(t *testing.T) {
	var h latency
	h.record(75 * time.Millisecond)
	h.record(6 * time.Second)
	var out strings.Builder
	h.render(&out, "latency")
	for _, line := range []string{"latency_bucket{le=\"0.05\"} 0\n", "latency_bucket{le=\"0.1\"} 1\n", "latency_bucket{le=\"+Inf\"} 2\n", "latency_count 2\n", "latency_sum 6.075\n"} {
		if !strings.Contains(out.String(), line) {
			t.Fatal(line, out.String())
		}
	}
}
