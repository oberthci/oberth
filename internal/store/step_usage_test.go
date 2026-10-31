package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/model"
)

func TestStepUsageNullZeroAndMeasurementsSurviveReopen(t *testing.T) {
	ctx := context.Background()
	name := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(ctx, name, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			_ = s.Close()
		}
	}()
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "usage", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	for i, step := range []model.StepResult{
		{Step: "unknown"},
		{Step: "zero", MaxRSSBytes: ptr.To(int64(0)), MemoryPeakBytes: ptr.To(int64(0)), UserCPU: ptr.To(time.Duration(0)), SystemCPU: ptr.To(time.Duration(0))},
		{Step: "measured", MaxRSSBytes: ptr.To(int64(64 << 20)), MemoryPeakBytes: ptr.To(int64(90 << 20)), UserCPU: ptr.To(2 * time.Second), SystemCPU: ptr.To(25 * time.Millisecond)},
	} {
		step.RunID, step.Burn, step.Status, step.DeclaredSize, step.Ordinal = run.ID, "test", model.StepPassed, "M", i
		if _, err := s.PutStepResult(ctx, step); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = nil
	s, err = Open(ctx, name, Options{})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.StepResults(ctx, run.ID)
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows=%+v %v", rows, err)
	}
	unknown, zero, known := rows[0], rows[1], rows[2]
	raw, _ := json.Marshal(unknown)
	for _, field := range []string{"MaxRSSBytes", "MemoryPeakBytes", "UserCPU", "SystemCPU"} {
		if !strings.Contains(string(raw), `"`+field+`":null`) {
			t.Fatalf("unknown is not null: %s", raw)
		}
	}
	if zero.MaxRSSBytes == nil || *zero.MaxRSSBytes != 0 || zero.MemoryPeakBytes == nil || *zero.MemoryPeakBytes != 0 || zero.UserCPU == nil || *zero.UserCPU != 0 || zero.SystemCPU == nil || *zero.SystemCPU != 0 {
		t.Fatalf("zero became unknown: %+v", zero)
	}
	if known.MaxRSSBytes == nil || *known.MaxRSSBytes != 64<<20 || known.MemoryPeakBytes == nil || *known.MemoryPeakBytes != 90<<20 || known.UserCPU == nil || *known.UserCPU != 2*time.Second || known.SystemCPU == nil || *known.SystemCPU != 25*time.Millisecond {
		t.Fatalf("measured %+v", known)
	}
	var nulls int
	if err := s.db.QueryRow(`SELECT count(*) FROM step_results WHERE step='unknown' AND max_rss_bytes IS NULL AND memory_peak_bytes IS NULL AND user_cpu_ns IS NULL AND system_cpu_ns IS NULL`).Scan(&nulls); err != nil || nulls != 1 {
		t.Fatalf("SQL unknowns: %d %v", nulls, err)
	}

	// Recovery without a retained Pod must not erase already-recorded counters.
	replay := known
	replay.MaxRSSBytes, replay.MemoryPeakBytes, replay.UserCPU, replay.SystemCPU = nil, nil, nil, nil
	replay, err = s.PutStepResult(ctx, replay)
	if err != nil || replay.MaxRSSBytes == nil || *replay.MaxRSSBytes != 64<<20 || replay.UserCPU == nil || *replay.UserCPU != 2*time.Second {
		t.Fatalf("replay erased usage: %+v %v", replay, err)
	}
	known.MaxRSSBytes = ptr.To(int64(-1))
	if _, err := s.PutStepResult(ctx, known); err == nil {
		t.Fatal("accepted negative usage")
	}
}

func TestStepUsageV32MigrationPreservesDataAndUnknowns(t *testing.T) {
	ctx := context.Background()
	name := filepath.Join(t.TempDir(), "legacy.db")
	s, err := Open(ctx, name, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			_ = s.Close()
		}
	}()
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "legacy", strings.Repeat("c", 40)))
	if err != nil {
		t.Fatal(err)
	}
	// Reconstitute the actual old NOT NULL/default-zero schema, not just its
	// ledger. This also covers migration from populated, recorded step results.
	_, err = s.db.Exec(`
DROP TABLE IF EXISTS step_resource_usage; DROP INDEX IF EXISTS runs_latest_ref_idx;
DROP TABLE IF EXISTS run_priority_overrides;
DROP TABLE IF EXISTS queue_priority_patterns;
DROP TABLE step_results;
CREATE TABLE step_results (
 run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
 burn TEXT NOT NULL,step TEXT NOT NULL,ordinal INTEGER NOT NULL,
 status TEXT NOT NULL CHECK(status IN ('passed','failed','skipped','timed_out')),
 exit_code INTEGER NOT NULL,log_start INTEGER NOT NULL CHECK(log_start>=0),
 log_end INTEGER NOT NULL CHECK(log_end>=log_start),
 started_at INTEGER,finished_at INTEGER,recorded_at INTEGER NOT NULL,
 declared_size TEXT NOT NULL DEFAULT 'M' CHECK(declared_size IN ('S','M','L','XL')),
 max_rss_bytes INTEGER NOT NULL DEFAULT 0 CHECK(max_rss_bytes>=0),
 user_cpu_ns INTEGER NOT NULL DEFAULT 0 CHECK(user_cpu_ns>=0),
 system_cpu_ns INTEGER NOT NULL DEFAULT 0 CHECK(system_cpu_ns>=0),
 termination_json TEXT CHECK(termination_json IS NULL OR length(termination_json)<=4096),
 PRIMARY KEY(run_id,burn,step));
CREATE INDEX step_results_order_idx ON step_results(run_id,ordinal,burn,step);
DELETE FROM schema_migrations WHERE version>=33;`)
	if err != nil {
		t.Fatal(err)
	}
	for i, step := range []string{"unknown", "known"} {
		_, err = s.db.Exec(`INSERT INTO step_results(run_id,burn,step,ordinal,status,exit_code,log_start,log_end,recorded_at,max_rss_bytes,user_cpu_ns,system_cpu_ns) VALUES(?,'test',?,?,'passed',0,12,42,1000,?,0,0)`, run.ID, step, i, i*8192)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s = nil
	s, err = Open(ctx, name, Options{})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.StepResults(ctx, run.ID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows %+v %v", rows, err)
	}
	if rows[0].MaxRSSBytes != nil || rows[0].UserCPU != nil || rows[0].SystemCPU != nil || rows[0].MemoryPeakBytes != nil {
		t.Fatal("legacy zero fabricated measurement")
	}
	if rows[1].MaxRSSBytes == nil || *rows[1].MaxRSSBytes != 8192 || rows[1].UserCPU != nil || rows[1].SystemCPU != nil || rows[1].MemoryPeakBytes != nil || rows[1].LogStart != 12 || rows[1].LogEnd != 42 {
		t.Fatalf("legacy measured record lost: %+v", rows[1])
	}
	if _, err = s.db.Exec(`DELETE FROM runs WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	rows, err = s.StepResults(ctx, run.ID)
	if err != nil || len(rows) != 0 {
		t.Fatal("ON DELETE CASCADE lost")
	}
}
