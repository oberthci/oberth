package store

import (
	"fmt"
	"path/filepath"
	"testing"
)

// Run at the default and maximum configurable event retention. A reader at the
// tail must not pay for every historical row on each event or fallback poll.
func BenchmarkAgentEventBounds(b *testing.B) {
	for _, count := range []int{10000, 1000000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			s, err := CreateGenesis(b.Context(), filepath.Join(b.TempDir(), "events.db"), Options{})
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			_, err = s.db.ExecContext(b.Context(), `WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<?)
INSERT INTO agent_events(dedupe_key, kind, repo_id, repo, run_id, promotion_id, ref, sha, summary, data, created_at)
SELECT printf('event-%d',i), 'run.finished', 1, 'forge/repo', '', '', '', '', '', '', 1 FROM n`, count)
			if err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			b.ReportAllocs()
			for b.Loop() {
				oldest, newest, err := s.AgentEventBounds(b.Context())
				if err != nil || oldest != 1 || newest != int64(count) {
					b.Fatalf("bounds %d..%d: %v", oldest, newest, err)
				}
			}
		})
	}
}
