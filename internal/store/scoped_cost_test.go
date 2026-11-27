package store

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

// Compare at a fixed iteration count on the same host. Ten percent is the
// declared noise allowance; this measures S0, without a Node or receipt claim.
func BenchmarkScopedCommitMatrix(b *testing.B) {
	for _, observers := range []int{30, 100} {
		for _, matching := range []int{10, observers} {
			b.Run(fmt.Sprintf("observers%d/matching%d", observers, matching), func(b *testing.B) {
				benchmarkScopedCommit(b, observers, matching)
			})
		}
	}
}

func benchmarkScopedCommit(b *testing.B, observers, matching int) {
	s, err := Create(b.Context(), filepath.Join(b.TempDir(), "state.sqlite"), testOptions())
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			b.Error(err)
		}
	}()
	subs := make([]stateport.Subscription, observers)
	for i := range subs {
		id := fmt.Sprint("unrelated-", i)
		if i < matching {
			id = "own"
		}
		subs[i] = s.Subscribe(stateport.ChangeScope{Runs: []string{id}})
		defer subs[i].Close()
	}
	var cursor int64
	readers := scopedReaders{store: s, subs: subs, matching: matching}
	latencies := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for i := range b.N {
		start := b.Elapsed()
		e := stateport.EventWrite{Dedupe: fmt.Sprint("event-", i), Event: model.AgentEvent{Repo: "origin/team/repo", RunID: "own", Kind: "run.updated"}}
		if err := s.RecordEvents(b.Context(), []stateport.EventWrite{e}); err != nil {
			b.Fatal(err)
		}
		latencies = append(latencies, b.Elapsed()-start)
		cursor = readers.readNotified(b, cursor)
	}
	b.StopTimer()
	slices.Sort(latencies)
	b.ReportMetric(float64(latencies[(len(latencies)-1)*95/100])/float64(time.Millisecond), "p95-write-ms")
	slices.Sort(readers.latencies)
	b.ReportMetric(float64(readers.latencies[(len(readers.latencies)-1)*95/100])/float64(time.Millisecond), "p95-read-ms")
	b.ReportMetric(float64(matching), "reads/op")
	b.ReportMetric(0, "unrelated-reads/op")
	if info, err := os.Stat(s.path + "-wal"); err == nil {
		b.ReportMetric(float64(info.Size())/float64(b.N), "wal-bytes/op")
	}
}

type scopedReaders struct {
	store     *Store
	subs      []stateport.Subscription
	matching  int
	latencies []time.Duration
}

func (r *scopedReaders) readNotified(b *testing.B, cursor int64) int64 {
	b.Helper()
	var latest int64
	count := 0
	for i, sub := range r.subs {
		select {
		case <-sub.C:
			if i >= r.matching {
				b.Fatal("unrelated observer reread")
			}
			start := b.Elapsed()
			page, err := r.store.Events(b.Context(), stateport.EventFilter{RunIDs: []string{"own"}, After: cursor, Limit: 500})
			r.latencies = append(r.latencies, b.Elapsed()-start)
			count++
			if err != nil || len(page.Events) != 1 {
				b.Fatalf("scoped read: %+v %v", page, err)
			}
			latest = page.NextAfterID
		default:
		}
	}
	if count != r.matching {
		b.Fatal("matching observer missed its committed event", count)
	}
	return latest
}
