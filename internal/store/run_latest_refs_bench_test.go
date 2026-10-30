package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func latestRefLargeFixture(tb testing.TB) *Store {
	tb.Helper()
	s, err := Open(context.Background(), filepath.Join(tb.TempDir(), "large.db"), Options{})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if err := s.Close(); err != nil {
			tb.Error(err)
		}
	})
	up, err := s.CreateUpstream(context.Background(), model.UpstreamSpec{Name: "forge", Kind: "forgejo", BaseURL: "https://forge.test"})
	if err != nil {
		tb.Fatal(err)
	}
	repo, err := s.CreateRepository(context.Background(), model.RepositorySpec{Name: "repo", UpstreamID: up.ID, DefaultBranch: "main"})
	if err != nil {
		tb.Fatal(err)
	}
	_, err = s.db.ExecContext(context.Background(), `WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<50000)
 INSERT INTO runs(id,repo_id,ref_kind,ref,sha,actor,trigger,phase,status,error,queued_at,created_at,updated_at)
 SELECT printf('%032x',i),?,'branch',printf('ref-%04d',i%2000),?,'fixture','branch','job','failed',?,1,1,1 FROM n`, repo.ID, strings.Repeat("a", 40), strings.Repeat("e", 2048))
	if err != nil {
		tb.Fatal(err)
	}
	return s
}

func TestLatestRefLargeHistoryPreservesExactHeadsAndCounts(t *testing.T) {
	s := latestRefLargeFixture(t)
	start := time.Now()
	runs, err := s.ListRecentRuns(t.Context(), model.RunListFilter{LatestPerRef: true, Limit: 100})
	if err != nil || len(runs) != 100 {
		t.Fatal(len(runs), err)
	}
	for i, run := range runs {
		if run.QueueSequence != int64(50000-i) || run.EarlierAttempts != 24 || len(run.Error) != 2048 {
			t.Fatal("wrong grouped head", run.ID, run.QueueSequence, run.EarlierAttempts)
		}
	}
	t.Logf("50k rows / 2k refs / 2KiB errors: %s", time.Since(start))
	rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+latestRefQuery("", priorityOrderSQL(s.now())), 0, 0, "", "", 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "COVERING INDEX runs_latest_ref_idx") {
			found = true
		}
		if strings.Contains(detail, "TEMP B-TREE FOR GROUP BY") {
			t.Fatal("grouping sorts history", detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("history scan is not covered by the ref index")
	}
}

func BenchmarkLatestRefRuns50K(b *testing.B) {
	s := latestRefLargeFixture(b)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		runs, err := s.ListRecentRuns(b.Context(), model.RunListFilter{LatestPerRef: true, Limit: 100})
		if err != nil || len(runs) != 100 {
			b.Fatal(len(runs), err)
		}
	}
}
