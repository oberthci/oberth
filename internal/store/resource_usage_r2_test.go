package store

import (
	"context"
	"fmt"
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
	// Replicate valid observations with distinct IDs without making the cap
	// fixture itself execute thousands of application transactions.
	_, err := s.db.Exec(`WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n<?)
INSERT INTO step_resource_usage(run_id,repo_id,node_id,pod_uid,container_id,burn,step,last_observed_at,observation_json)
SELECT run_id,repo_id,node_id,pod_uid,'containerd://'||printf('%064x',n),burn,step,last_observed_at+n,
json_set(observation_json,'$.container_id','containerd://'||printf('%064x',n)) FROM step_resource_usage,seq WHERE container_id=?`, n-1, v.ContainerID)
	if err != nil {
		t.Fatal(err)
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
	// Occupy the sole primary connection without taking a file write lock.
	// Cleanup must proceed using its own writer, not wait for this pool slot.
	primary, err := s.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	deleted, err := s.PruneResourceUsage(t.Context())
	if err != nil || deleted != resourceUsageSweepRows {
		t.Fatal("sweep cap/connection isolation", deleted, err)
	}
	var count int
	var oldest int64
	if err := s.reader().QueryRow(`SELECT count(*),min(last_observed_at) FROM step_resource_usage`).Scan(&count, &oldest); err != nil || count != 2 || oldest != unixNano(old)+resourceUsageSweepRows {
		t.Fatal("not oldest first", count, oldest, err)
	}
	deleted, err = s.PruneResourceUsage(t.Context())
	if err != nil || deleted != 2 {
		t.Fatal("backlog did not drain", deleted, err)
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
