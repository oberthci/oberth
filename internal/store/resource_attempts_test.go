package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

const attemptInventoryMigration = "20261104220233-resource-attempt-inventory-53d49c721cd86410289f1448a4686d70"

func TestAttemptInventoryRetainsSupersededFailureAndRejectsRebinding(t *testing.T) {
	s, run, at := resourceStoreFixture(t)
	v := coveredResourceRow(run.ID, at)
	i := model.ResourceAttemptInventory{RunID: run.ID, WorkflowName: v.WorkflowName, WorkflowUID: v.WorkflowUID, Namespace: v.Namespace, Complete: true, Terminal: true, Attempts: []model.ResourceAttempt{{NodeID: "missing-retry", Stage: v.Stage, Step: v.Step, Template: v.TemplateName, Status: model.StepFailed}, {NodeID: v.NodeID, Stage: v.Stage, Step: v.Step, Template: v.TemplateName, Status: model.StepPassed, PodUID: v.PodUID}}}
	for range 2 {
		if err := s.RecordResourceAttempts(t.Context(), i); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordStepResourceUsage(t.Context(), []model.StepResourceUsage{v}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE runs SET status='passed' WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	// A late success for the same node and a snapshot omitting the old node
	// must both preserve the failed attempt. A retry has its own node key.
	i.Attempts[0].Status = model.StepPassed
	if err := s.RecordResourceAttempts(t.Context(), i); err != nil {
		t.Fatal(err)
	}
	i.Attempts = i.Attempts[1:]
	if err := s.RecordResourceAttempts(t.Context(), i); err != nil {
		t.Fatal(err)
	}
	h, err := s.RunResources(t.Context(), run.RepoID, RunResourceFilter{RunID: run.ID, Runs: 1})
	if err != nil || h.SafetyIncomplete || len(h.AttemptGaps) != 1 || h.AttemptGaps[0].NodeID != "missing-retry" || len(h.Observations) != 1 {
		t.Fatalf("missing retry borrowed coverage: %+v %v", h, err)
	}
	var raw string
	if err := s.db.QueryRow(`SELECT observation_json FROM resource_attempts WHERE run_id=? AND node_id='missing-retry'`, run.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "pod_uid") || strings.Contains(raw, "time") || strings.Contains(raw, "OOM") || strings.Contains(raw, "sample") {
		t.Fatal("missing Pod acquired invented identity, time, physical cause or measurements", raw)
	}
	for _, kind := range []string{"workflow", "pod", "invocation"} {
		bad := i
		bad.Attempts = append([]model.ResourceAttempt{}, i.Attempts...)
		switch kind {
		case "workflow":
			bad.WorkflowUID = "33333333-3333-4333-8333-333333333333"
		case "pod":
			bad.Attempts[0].PodUID = "33333333-3333-4333-8333-333333333333"
		case "invocation":
			bad.Attempts[0].Step = "foreign"
		}
		if s.RecordResourceAttempts(t.Context(), bad) == nil {
			t.Fatal("accepted changed", kind)
		}
	}
	other, err := s.RunResources(t.Context(), run.RepoID+1, RunResourceFilter{RunID: run.ID, Runs: 1})
	if err != nil || len(other.AttemptGaps) != 0 || other.SafetyIncomplete {
		t.Fatal("attempt evidence crossed repository boundary", err)
	}
	if _, err := s.db.Exec(`DELETE FROM runs WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"resource_attempts", "resource_attempt_inventories"} {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatal("run retention did not delete", table, n, err)
		}
	}
}

func TestAttemptInventoryCapPersistsIncompleteFence(t *testing.T) {
	s, run, _ := resourceStoreFixture(t)
	i := model.ResourceAttemptInventory{RunID: run.ID, WorkflowName: "workflow-test", WorkflowUID: "11111111-1111-4111-8111-111111111111", Namespace: "argo", Complete: true}
	for batch := range 3 {
		i.Attempts = nil
		for n := range 1500 {
			i.Attempts = append(i.Attempts, model.ResourceAttempt{NodeID: fmt.Sprintf("node-%d", batch*1500+n), Stage: "build", Step: "compile", Template: "compile", Status: model.StepRunning})
		}
		if err := s.RecordResourceAttempts(t.Context(), i); err != nil {
			t.Fatal(err)
		}
	}
	var raw string
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM resource_attempts WHERE run_id=?`, run.ID).Scan(&n); err != nil || n != 4096 {
		t.Fatal("inventory unbounded", n, err)
	}
	if err := s.db.QueryRow(`SELECT observation_json FROM resource_attempt_inventories WHERE run_id=?`, run.ID).Scan(&raw); err != nil || json.Unmarshal([]byte(raw), &i) != nil || i.Complete {
		t.Fatal("overflow silently dropped known attempts", raw, err)
	}
}

func TestAttemptMigrationUpgradesExactPredecessorOnce(t *testing.T) {
	s := newMigrationIDTestStore(t)
	var old []migration
	for _, m := range migrations {
		if m.ID() != attemptInventoryMigration {
			old = append(old, m)
		}
	}
	if err := s.migrateWithMigrations(t.Context(), nil, old); err != nil {
		t.Fatal(err)
	}
	ledger := func() string {
		var value string
		if err := s.db.QueryRow(`SELECT group_concat(id||':'||version||':'||applied_at,';') FROM (SELECT * FROM schema_migrations WHERE id<>? ORDER BY version)`, attemptInventoryMigration).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := ledger()
	for range 2 {
		if err := s.migrateWithMigrations(t.Context(), nil, migrations); err != nil {
			t.Fatal(err)
		}
	}
	if ledger() != before {
		t.Fatal("upgrade changed predecessor receipts")
	}
	if _, _, err := readAppliedMigrationsWith(t.Context(), s.db, old); err == nil {
		t.Fatal("predecessor accepted new schema")
	}
}
