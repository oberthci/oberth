package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func resourceStoreFixture(t *testing.T) (*Store, model.Run, time.Time) {
	t.Helper()
	at := time.Now().UTC()
	s := testStore(t, &at)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "feature/resource-r2", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetRunJobName(t.Context(), run.ID, "workflow-test"); err != nil {
		t.Fatal(err)
	}
	return s, run.Run, at
}

func TestResourceTerminalRepeatHasNoWrites(t *testing.T) {
	s, run, at := resourceStoreFixture(t)
	v := resourceRow(run.ID, at)
	v.Version = 2
	v.ContainerKind = "app"
	v.PodKind = "workflow"
	v.Terminal = true
	v.Reason = "Completed"
	code := int32(0)
	v.ExitCode = &code
	v.ContainerStartedAt = at.Add(-time.Minute)
	v.ContainerFinishedAt = at
	if err := s.RecordStepResourceUsage(t.Context(), []model.StepResourceUsage{v}); err != nil {
		t.Fatal(err)
	}
	var before, after string
	if err := s.db.QueryRow(`SELECT observation_json FROM step_resource_usage`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	// A trigger makes any attempted rewrite observable, including a byte-identical
	// UPDATE. The second pass may select/merge, but must execute no row write.
	if _, err := s.db.Exec(`CREATE TRIGGER resource_no_terminal_rewrite BEFORE UPDATE ON step_resource_usage BEGIN SELECT RAISE(ABORT,'terminal rewritten'); END`); err != nil {
		t.Fatal(err)
	}
	v.LastObservedAt = at.Add(time.Minute)
	if err := s.RecordStepResourceUsage(t.Context(), []model.StepResourceUsage{v}); err != nil {
		t.Fatal("terminal row rewritten", err)
	}
	if err := s.db.QueryRow(`SELECT observation_json FROM step_resource_usage`).Scan(&after); err != nil || before != after {
		t.Fatal("terminal JSON changed", err)
	}
}

func seedResourceRows(t *testing.T, s *Store, runID string, at time.Time, n int) {
	t.Helper()
	v := resourceRow(runID, at)
	if err := s.RecordStepResourceUsage(t.Context(), []model.StepResourceUsage{v}); err != nil {
		t.Fatal(err)
	}
	var observation string
	if err := s.db.QueryRow(`SELECT observation_json FROM step_resource_usage WHERE container_id=?`, v.ContainerID).Scan(&observation); err != nil {
		t.Fatal(err)
	}
	// Keep the complete valid observation and change only its container ID.
	// Parsing and rewriting this JSON in SQLite for each of 100,000 rows makes
	// the race suite spend minutes constructing the retention fixture.
	needle := `"container_id":"` + v.ContainerID + `"`
	prefix, suffix, found := strings.Cut(observation, needle)
	if !found || strings.Contains(suffix, needle) {
		t.Fatal("resource fixture must contain exactly one container_id field")
	}
	prefix += `"container_id":"containerd://`
	suffix = `"` + suffix
	// Replicate valid observations with distinct IDs without making the cap
	// fixture itself execute thousands of application transactions.
	_, err := s.db.Exec(`WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n<?)
INSERT INTO step_resource_usage(run_id,repo_id,node_id,pod_uid,container_id,burn,step,last_observed_at,observation_json)
SELECT run_id,repo_id,node_id,pod_uid,'containerd://'||printf('%064x',n),burn,step,last_observed_at+n,
?||printf('%064x',n)||? FROM step_resource_usage,seq WHERE container_id=?`, n-1, prefix, suffix, v.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
}

func TestSeedResourceRowsPreservesObservations(t *testing.T) {
	s, run, at := resourceStoreFixture(t)
	seedResourceRows(t, s, run.ID, at, 3)
	rows, err := s.db.Query(`SELECT container_id,observation_json FROM step_resource_usage ORDER BY container_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id, observation string
		if err := rows.Scan(&id, &observation); err != nil {
			t.Fatal(err)
		}
		var got model.StepResourceUsage
		if err := json.Unmarshal([]byte(observation), &got); err != nil {
			t.Fatal(err)
		}
		want := resourceRow(run.ID, at)
		want.ContainerID = id
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("fixture changed observation fields: got %#v, want %#v", got, want)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{fmt.Sprintf("containerd://%064x", 1), fmt.Sprintf("containerd://%064x", 2), resourceRow(run.ID, at).ContainerID}
	if !slices.Equal(ids, want) {
		t.Fatalf("fixture IDs = %v, want %v", ids, want)
	}
}

func TestResourceRunCapKeepsKnownIncarnationsUpdatable(t *testing.T) {
	s, run, at := resourceStoreFixture(t)
	seedResourceRows(t, s, run.ID, at, 4096)
	v := resourceRow(run.ID, at)
	v.ContainerID = "containerd://" + strings.Repeat("f", 64)
	if err := s.RecordStepResourceUsage(t.Context(), []model.StepResourceUsage{v}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM step_resource_usage`).Scan(&count); err != nil || count != 4096 {
		t.Fatal("row cap removed", count, err)
	}
	v = resourceRow(run.ID, at)
	peak := int64(999)
	v.MemoryPeakBytes = &peak
	v.LastObservedAt = at.Add(time.Second)
	if err := s.RecordStepResourceUsage(t.Context(), []model.StepResourceUsage{v}); err != nil {
		t.Fatal(err)
	}
	var peakStored int64
	if err := s.db.QueryRow(`SELECT json_extract(observation_json,'$.memory_peak_bytes') FROM step_resource_usage WHERE container_id=?`, v.ContainerID).Scan(&peakStored); err != nil || peakStored != 999 {
		t.Fatal("cap blocked existing row", peakStored, err)
	}
}

func TestResourceRetentionOldestFirstBoundedAndIndependentOfPrimary(t *testing.T) {
	s, run, at := resourceStoreFixture(t)
	old := at.Add(-resourceUsageMaxAge - time.Hour)
	seedResourceRows(t, s, run.ID, old, resourceUsageSweepRows+2)
	// Timestamp order must differ from insertion order. Groups of three also
	// put equal timestamps across the 100-row delete and 1,000-row sweep caps.
	if _, err := s.db.Exec(`UPDATE step_resource_usage SET last_observed_at=?+(?-rowid)/3`, unixNano(old), resourceUsageSweepRows+2); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	snapshot := func() []resourceUsageCandidate {
		t.Helper()
		rows, err := s.reader().QueryContext(ctx, `SELECT rowid,last_observed_at FROM step_resource_usage ORDER BY last_observed_at,rowid`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var values []resourceUsageCandidate
		for rows.Next() {
			var v resourceUsageCandidate
			if err := rows.Scan(&v.id, &v.at); err != nil {
				t.Fatal(err)
			}
			values = append(values, v)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return values
	}
	expected := snapshot()
	if len(expected) != resourceUsageSweepRows+2 {
		t.Fatal("retention fixture row count", len(expected))
	}
	// Occupy the sole primary connection without taking a file write lock.
	// Cleanup must proceed using its own writer, not wait for this pool slot.
	primary, err := s.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	// The production one-second budget may expire under race instrumentation
	// or host load. Check ordering/caps after every partial sweep and allow
	// bounded catch-up instead of requiring all 1,000 deletes in one budget.
	total := 0
	for pass := 0; pass < 20 && total < len(expected); pass++ {
		deleted, err := s.PruneResourceUsage(ctx)
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("sweep/connection isolation", deleted, err)
		}
		if deleted < 0 || deleted > resourceUsageSweepRows {
			t.Fatal("sweep cap", deleted)
		}
		remaining := snapshot()
		observedTotal := len(expected) - len(remaining)
		observedDeleted := int64(observedTotal - total)
		// SQLite may commit a DELETE before cancellation makes ExecContext
		// return a deadline error instead of its RowsAffected result. Only
		// that final batch can be missing from the acknowledged count.
		if observedDeleted < deleted || observedDeleted > resourceUsageSweepRows ||
			(err == nil && observedDeleted != deleted) || observedDeleted-deleted > resourceUsageDeleteBatch {
			t.Fatal("sweep count", observedDeleted, deleted, err)
		}
		if observedDeleted != deleted {
			t.Logf("deadline after committed batch: acknowledged=%d persisted=%d", deleted, observedDeleted)
		}
		// Verify every surviving row, including the rowid tie-breaker. The
		// persisted suffix is authoritative even when the last count is lost.
		if !slices.Equal(remaining, expected[observedTotal:]) {
			t.Fatal("partial sweep was not oldest first", len(remaining), observedTotal, deleted, err)
		}
		total = observedTotal
	}
	if total != resourceUsageSweepRows+2 {
		t.Fatal("bounded backlog did not drain while primary pool slot held", total)
	}
}

func TestResourceRetentionGlobalTargetAndFreshRows(t *testing.T) {
	s, run, at := resourceStoreFixture(t)
	seedResourceRows(t, s, run.ID, at, resourceUsageRetainedRows+3)
	n, err := s.PruneResourceUsage(t.Context())
	if err != nil || n != 3 {
		t.Fatal("global target not enforced", n, err)
	}
	n, err = s.PruneResourceUsage(t.Context())
	if err != nil || n != 0 {
		t.Fatal("fresh retained rows removed", n, err)
	}
}

func TestResourceRetentionYieldsToPrimaryWriteLock(t *testing.T) {
	s, run, at := resourceStoreFixture(t)
	seedResourceRows(t, s, run.ID, at.Add(-resourceUsageMaxAge-time.Hour), 2)
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	n, err := s.PruneResourceUsage(ctx)
	if n != 0 || err == nil || ctx.Err() != nil {
		t.Fatal("retention did not yield at short SQLite busy timeout", n, err, ctx.Err())
	}
}

func TestResourceRunViewPrioritizesMainBeforeHelpers(t *testing.T) {
	s, run, at := resourceStoreFixture(t)
	seedResourceRows(t, s, run.ID, at, 257)
	if _, err := s.db.Exec(`UPDATE step_resource_usage SET observation_json=json_set(observation_json,'$.version',2,'$.container_kind','app','$.pod_kind','workflow','$.container_name','wait')`); err != nil {
		t.Fatal(err)
	}
	last := fmt.Sprintf("containerd://%064x", 256)
	if _, err := s.db.Exec(`UPDATE step_resource_usage SET observation_json=json_set(observation_json,'$.container_name','main') WHERE container_id=?`, last); err != nil {
		t.Fatal(err)
	}
	rows, more, err := s.StepResourceUsage(t.Context(), run.ID)
	if err != nil || !more || len(rows) != 256 || !rows[0].IsWorkflowMain() {
		t.Fatal("helpers displaced main", len(rows), more, err)
	}
}

func TestResourceRetentionDeleteRechecksRenewedClock(t *testing.T) {
	s, run, at := resourceStoreFixture(t)
	old := at.Add(-resourceUsageMaxAge - time.Hour)
	seedResourceRows(t, s, run.ID, old, 2)
	rows, err := s.reader().QueryContext(t.Context(), `SELECT rowid,last_observed_at FROM step_resource_usage ORDER BY last_observed_at`)
	if err != nil {
		t.Fatal(err)
	}
	var selected []resourceUsageCandidate
	for rows.Next() {
		var v resourceUsageCandidate
		if err := rows.Scan(&v.id, &v.at); err != nil {
			t.Fatal(err)
		}
		selected = append(selected, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	// Selection has finished; the current incarnation is renewed before the
	// production DELETE predicate obtains a writer reservation.
	v := resourceRow(run.ID, at)
	if err := s.RecordStepResourceUsage(t.Context(), []model.StepResourceUsage{v}); err != nil {
		t.Fatal(err)
	}
	deleted, err := deleteResourceUsageBatch(t.Context(), s.db, selected)
	if err != nil || deleted != 1 {
		t.Fatal("renewed observation deleted", deleted, err)
	}
	values, _, err := s.StepResourceUsage(t.Context(), run.ID)
	if err != nil || len(values) != 1 || values[0].ContainerID != v.ContainerID || !values[0].LastObservedAt.Equal(at) {
		t.Fatal("DELETE failed to protect renewed clock", values, err)
	}
}

func TestResourceRetentionDeleteMatchesRowAndClockPairwise(t *testing.T) {
	s, run, at := resourceStoreFixture(t)
	seedResourceRows(t, s, run.ID, at, 2)
	rows, err := s.reader().QueryContext(t.Context(), `SELECT rowid,last_observed_at FROM step_resource_usage ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var selected []resourceUsageCandidate
	for rows.Next() {
		var v resourceUsageCandidate
		if err := rows.Scan(&v.id, &v.at); err != nil {
			t.Fatal(err)
		}
		selected = append(selected, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if len(selected) != 2 || selected[0].at == selected[1].at {
		t.Fatal("fixture needs two distinct row/clock pairs", selected)
	}
	// Both IDs and both clocks exist, but neither crossed tuple exists. Two
	// independent IN predicates would incorrectly delete both observations.
	crossed := []resourceUsageCandidate{{id: selected[0].id, at: selected[1].at}, {id: selected[1].id, at: selected[0].at}}
	if deleted, err := deleteResourceUsageBatch(t.Context(), s.db, crossed); err != nil || deleted != 0 {
		t.Fatal("crossed row/clock pairs deleted observations", deleted, err)
	}
	var remaining int
	if err := s.reader().QueryRowContext(t.Context(), `SELECT count(*) FROM step_resource_usage`).Scan(&remaining); err != nil || remaining != 2 {
		t.Fatal("crossed pairs changed retained rows", remaining, err)
	}
	// Positive control: the original matching tuples must still delete both.
	if deleted, err := deleteResourceUsageBatch(t.Context(), s.db, selected); err != nil || deleted != 2 {
		t.Fatal("matching row/clock pairs did not delete both observations", deleted, err)
	}
}

func TestResourceRetentionCommitInvalidatesAuditCache(t *testing.T) {
	s, run, at := resourceStoreFixture(t)
	seedAuditMetricRows(t, s, 32, at)
	localGate(t, s)
	localGate(t, s)
	before := s.localAudit.fullScans
	if before != 1 {
		t.Fatal("cache did not warm", before)
	}
	if n, err := s.PruneResourceUsage(t.Context()); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	localGate(t, s)
	if s.localAudit.fullScans != before {
		t.Fatal("empty sweep invalidated cache")
	}
	seedResourceRows(t, s, run.ID, at.Add(-resourceUsageMaxAge-time.Hour), 2)
	localGate(t, s)
	before = s.localAudit.fullScans
	if n, err := s.PruneResourceUsage(t.Context()); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	localGate(t, s)
	if s.localAudit.fullScans != before+1 {
		t.Fatal("separate retention commit did not reset primary audit cache", before, s.localAudit.fullScans)
	}
}
