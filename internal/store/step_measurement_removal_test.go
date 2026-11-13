package store

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestStepMeasurementRemovalDeletesHistoryAndPreservesResults(t *testing.T) {
	const id = "20261113013336-remove-step-measurement-32ae7f0f09084618bddcb8f20fd0fbac"
	s := newMigrationIDTestStore(t)
	ctx := t.Context()
	index := -1
	for i, m := range migrations {
		if m.id == id {
			index = i
		}
	}
	if index < 0 {
		t.Fatal("measurement removal missing", index)
	}
	if err := s.migrateWithMigrations(ctx, nil, migrations[:index]); err != nil {
		t.Fatal(err)
	}
	if err := s.configureLiveReaders(ctx, 0); err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/measurements", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`INSERT INTO step_results(run_id,burn,step,ordinal,status,exit_code,log_start,log_end,started_at,finished_at,recorded_at,declared_size,max_rss_bytes,memory_peak_bytes,user_cpu_ns,system_cpu_ns,termination_json)
 VALUES(?,'test','unit',2,'failed',137,11,42,100,200,300,'XL',1024,2048,3000,4000,'{"available":false,"unavailable":"not-recorded"}')`, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO step_resource_usage VALUES(?,?,'node','pod','container','test','unit',123,'{}')`,
		`INSERT INTO resource_attempt_inventories VALUES(?,?,'{}')`,
		`INSERT INTO pod_resource_dispositions VALUES(?,?,'node','pod','test','unit','{}')`,
	} {
		if _, err = s.db.Exec(q, run.ID, repo.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.db.Exec(`INSERT INTO resource_attempts VALUES(?,'node','{}')`, run.ID); err != nil {
		t.Fatal(err)
	}
	audit, err := s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = s.migrateWithMigrations(ctx, nil, migrations); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.StepResults(ctx, run.ID)
	if err != nil || len(got) != 1 {
		t.Fatal(got, err)
	}
	step := got[0]
	if step.Status != model.StepFailed || step.ExitCode != 137 || step.LogStart != 11 || step.LogEnd != 42 || step.Ordinal != 2 || step.StartedAt.UnixNano() != 100 || step.FinishedAt.UnixNano() != 200 || step.RecordedAt.UnixNano() != 300 || step.Termination.Unavailable != "not-recorded" {
		t.Fatal("result changed", step)
	}
	after, err := s.VerifyAuditChain(ctx)
	if err != nil || !reflect.DeepEqual(audit, after) {
		t.Fatal("audit changed", err)
	}
	for _, table := range []string{"step_resource_usage", "resource_attempts", "resource_attempt_inventories", "pod_resource_dispositions"} {
		var count int
		if err = s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&count); err != nil || count != 0 {
			t.Fatal("measurement table survived", table, err)
		}
	}
	now := time.Now().UTC()
	result := model.StepResult{RunID: run.ID, Stage: "test", Step: "next", Ordinal: 3, Status: model.StepPassed, StartedAt: &now, FinishedAt: &now}
	if _, err = s.PutStepResult(ctx, result); err != nil {
		t.Fatal("active result write failed", err)
	}
	current, err := s.Run(ctx, run.ID)
	if err != nil || current.Status != run.Status {
		t.Fatal("run state changed", current, err)
	}
}
