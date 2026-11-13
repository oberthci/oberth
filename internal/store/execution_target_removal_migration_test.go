package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestExecutionTargetRemovalRefusesRemoteRecoveryAndPreservesLocalRuns(t *testing.T) {
	const id = "20261113211418-drop-execution-targets-0c8c62c219d8b3115aa50e87dde3701f"
	s := newMigrationIDTestStore(t)
	ctx := t.Context()
	index := -1
	for i, m := range migrations {
		if m.id == id {
			index = i
		}
	}
	if index < 0 {
		t.Fatal("migration missing")
	}
	if err := s.migrateWithMigrations(ctx, nil, migrations[:index]); err != nil {
		t.Fatal(err)
	}
	if err := s.configureLiveReaders(ctx, 0); err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	cases := []struct {
		name, target, status, phase, failure string
		cancel, refuse                       bool
	}{
		{"remote-queued", "remote", "queued", "queued", "", false, true},
		{"remote-quarantined", "remote", "running", "running", "", true, true},
		{"remote-running", "remote", "running", "running", "", false, true},
		{"remote-waiting", "remote", "running", "waiting", "", false, true},
		{"remote-pending", "remote", "running", "infrastructure-pending", "Workflow create outcome uncertain; cleanup pending", true, true},
		{"remote-interrupted-pending", "remote", "interrupted", "interrupted", ExecutorIdentityPendingMessage, true, true},
		{"remote-spec-pending", "remote", "interrupted", "interrupted", WorkflowSpecMismatchPendingMessage, true, true},
		{"remote-passed", "remote", "passed", "complete", "", false, false},
		{"remote-cancelled", "remote", "interrupted", "cancelled", "original failure", true, false},
		{"unknown-target", "", "running", "running", "", false, true},
		{"local-running", "local", "running", "running", "", false, false},
		{"local-queued", "local", "queued", "queued", "", false, false},
		{"local-pending", "local", "running", "infrastructure-pending", WorkflowSpecMismatchPendingMessage, true, false},
		{"unbound-running", "unbound", "running", "running", "", false, false},
	}
	before := map[string]model.Run{}
	for _, tc := range cases {
		spec := testRunSpec(repo.ID, tc.name, strings.Repeat("a", 40))
		spec.Trigger = "branch"
		enqueued, err := s.EnqueueRun(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.Exec(`UPDATE runs SET status=?,phase=?,error=?,job_name=?,finished_at=CASE WHEN ? IN ('passed','interrupted') THEN 123 ELSE NULL END WHERE id=?`, tc.status, tc.phase, tc.failure, tc.name, tc.status, enqueued.ID); err != nil {
			t.Fatal(err)
		}
		run, err := s.Run(ctx, enqueued.ID)
		if err != nil {
			t.Fatal(err)
		}
		before[tc.name] = run
		if tc.target != "unbound" {
			binding, err := json.Marshal(map[string]any{"RunID": run.ID, "Workflow": run.WorkflowName, "Target": tc.target, "Snapshot": map[string]any{}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.db.Exec(`INSERT INTO execution_target_bindings(run_id,workflow_name,binding_json,quarantine_reason) VALUES(?,?,?,?)`, run.ID, run.WorkflowName, string(binding), map[bool]string{false: "", true: "inconsistent-placement-custody"}[tc.name == "remote-quarantined"]); err != nil {
				t.Fatal(err)
			}
			if _, err = s.db.Exec(`INSERT INTO execution_target_resources(run_id,kind,namespace,name,uid,object_json,not_before) VALUES(?,'Workflow','original-cluster',?,'original-uid','{}',0)`, run.ID, run.WorkflowName); err != nil {
				t.Fatal(err)
			}
		}
		if tc.cancel {
			if _, err = s.db.Exec(`INSERT INTO run_cancellations(run_id,job_name,reason,created_at) VALUES(?,?,'owner_restart',123)`, run.ID, run.WorkflowName); err != nil {
				t.Fatal(err)
			}
		}
	}
	audit, err := s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ledger := releaseSplitLedger(t, s.db)
	for range 2 {
		if err = s.migrateWithMigrations(ctx, nil, migrations); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range cases {
		old := before[tc.name]
		got, err := s.Run(ctx, old.ID)
		if err != nil {
			t.Fatal(err)
		}
		if tc.refuse {
			if got.Status != model.RunFailed || got.Phase != "infrastructure" || !strings.Contains(got.Error, "recovery refused") || got.FinishedAt == nil {
				t.Fatalf("remote recovery not retired: %+v", got)
			}
			if got.ID != old.ID || got.WorkflowName != old.WorkflowName || got.SHA != old.SHA {
				t.Fatalf("run history changed: %+v", got)
			}
		} else if !reflect.DeepEqual(old, got) {
			t.Fatalf("unrelated run %s changed: before=%+v after=%+v", tc.name, old, got)
		}
	}
	active, err := s.RunningRunsWithJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 3 {
		t.Fatalf("local adoption candidates changed: %+v", active)
	}
	for _, run := range active {
		if run.WorkflowName != "local-running" && run.WorkflowName != "local-pending" && run.WorkflowName != "unbound-running" {
			t.Fatalf("remote run reached local adoption: %+v", run)
		}
	}
	pending, err := s.PendingRunCancellations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].RunID != before["local-pending"].ID {
		t.Fatalf("remote cancellation reached local cleanup: %+v", pending)
	}
	var remaining int
	if err = s.db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name LIKE 'execution_target_%'`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("custody schema survived: %d %v", remaining, err)
	}
	after, err := s.VerifyAuditChain(ctx)
	if err != nil || !reflect.DeepEqual(audit, after) {
		t.Fatal("audit history changed", err)
	}
	for prior, stamp := range ledger {
		if releaseSplitLedger(t, s.db)[prior] != stamp {
			t.Fatal("prior migration receipt changed", prior)
		}
	}
	rows, err := s.db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() || rows.Err() != nil {
		t.Fatal("foreign key violation", rows.Err())
	}
}
