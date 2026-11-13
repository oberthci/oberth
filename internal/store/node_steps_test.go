package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestNativeNodeStepsStore2000RowsInBoundedTransactions(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "fanout", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	rows := make([]model.StepResult, 2000)
	for i := range rows {
		rows[i] = model.StepResult{NodeID: fmt.Sprintf("node-%04d", i), Stage: "build", Step: "same", DisplayName: "same", Template: "leaf", NodeType: "Pod", Phase: "Pending", Status: model.StepQueued, Ordinal: i}
	}
	// Failing the first row after a complete batch demonstrates that the
	// writer commits bounded transactions, rather than one 2000-row write.
	if _, err := s.db.Exec(`CREATE TRIGGER fixture_step_batch_failure BEFORE INSERT ON step_results WHEN NEW.node_id='node-0128' BEGIN SELECT RAISE(ABORT,'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.PutStepResults(t.Context(), run.ID, rows); err == nil {
		t.Fatal("injected write failure was ignored")
	}
	stored, err := s.StepResults(t.Context(), run.ID)
	if err != nil || len(stored) != 128 {
		t.Fatalf("first batch did not commit independently: rows=%d error=%v", len(stored), err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fixture_step_batch_failure`); err != nil {
		t.Fatal(err)
	}
	if err := s.PutStepResults(t.Context(), run.ID, rows); err != nil {
		t.Fatal(err)
	}
	stored, err = s.StepResults(t.Context(), run.ID)
	if err != nil || len(stored) != 2000 {
		t.Fatalf("native nodes were capped or display names collapsed: rows=%d error=%v", len(stored), err)
	}
	rows[1234].Phase, rows[1234].Status, rows[1234].Message = "Succeeded", model.StepPassed, "native completion"
	if err := s.PutStepResults(t.Context(), run.ID, rows[1234:1235]); err != nil {
		t.Fatal(err)
	}
	stored, err = s.StepResults(t.Context(), run.ID)
	if err != nil || len(stored) != 2000 || stored[1234].Status != model.StepPassed || stored[1234].Message != "native completion" || stored[1235].Status != model.StepQueued {
		t.Fatal("node update affected a same-named sibling", err)
	}
}

func TestNativeNodeStepsMigrationRetainsPopulatedHistory(t *testing.T) {
	const id = "20261113214401-native-node-steps-c477a88045b8945eb269fade9eb006e2"
	s := newMigrationIDTestStore(t)
	index := -1
	for i, m := range migrations {
		if m.id == id {
			index = i
		}
	}
	if index < 0 {
		t.Fatal("native node migration missing")
	}
	if err := s.migrateWithMigrations(t.Context(), nil, migrations[:index]); err != nil {
		t.Fatal(err)
	}
	if err := s.configureLiveReaders(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "history", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO step_results(run_id,burn,step,ordinal,status,exit_code,log_start,log_end,started_at,finished_at,recorded_at) VALUES(?,'build','compile',3,'failed',17,12,42,100,200,300)`, run.ID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.migrateWithMigrations(t.Context(), nil, migrations); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.StepResults(t.Context(), run.ID)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	row := rows[0]
	if row.NodeID == "" || row.Step != "compile" || row.Stage != "build" || row.Ordinal != 3 || row.Status != model.StepFailed || row.ExitCode != 17 || row.StartedAt.UnixNano() != 100 || row.FinishedAt.UnixNano() != 200 || row.RecordedAt.UnixNano() != 300 || row.Phase != "" || row.Message != "" {
		t.Fatalf("retained history changed or native data invented: %+v", row)
	}
	var columns int
	if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('step_results') WHERE name IN ('log_start','log_end')`).Scan(&columns); err != nil || columns != 0 {
		t.Fatal("marker-derived columns survived", columns, err)
	}
}

func TestNodeCredentialEvidenceComesFromBoundAdmissionReceipt(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "evidence", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRunWorkflowName(t.Context(), run.ID, "workflow"); err != nil {
		t.Fatal(err)
	}
	appendReceipt := func(credentialed bool, workflow string) {
		t.Helper()
		grants := []string{}
		if credentialed {
			grants = []string{"secret:approved-path"}
		}
		body, err := json.Marshal(map[string]any{"engine": "argo", "workflow": workflow, "step_credentials": []model.StepCredentialEvidence{{Known: true, Complete: true, Template: "leaf", Credentialed: credentialed, Grants: grants}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.AppendAuditAction(t.Context(), model.AuditActionSpec{Actor: "actor", Action: "ci.argo.submit.binding", ResourceType: "run", ResourceID: run.ID, Details: string(body)}); err != nil {
			t.Fatal(err)
		}
	}
	row := model.StepResult{RunID: run.ID, NodeID: "node", Template: "leaf", LocalTemplate: true, Status: model.StepPassed, CredentialEvidenceKnown: true}
	// A producer cannot invent a credential-free fact without a receipt.
	stored, err := s.PutStepResult(t.Context(), row)
	if err != nil || stored.CredentialEvidenceKnown {
		t.Fatal(stored, err)
	}
	appendReceipt(false, "other-workflow")
	stored, err = s.PutStepResult(t.Context(), row)
	if err != nil || stored.CredentialEvidenceKnown {
		t.Fatal("foreign Workflow receipt used", stored, err)
	}
	appendReceipt(false, "workflow")
	stored, err = s.PutStepResult(t.Context(), row)
	if err != nil || !stored.CredentialEvidenceKnown || stored.Credentialed {
		t.Fatal(stored, err)
	}
	// A referenced template cannot borrow an inline template's same name.
	row.NodeID, row.LocalTemplate = "referenced-node", false
	stored, err = s.PutStepResult(t.Context(), row)
	if err != nil || stored.CredentialEvidenceKnown {
		t.Fatal("reference borrowed inline authority", stored, err)
	}
	row.NodeID, row.LocalTemplate = "node", true
	appendReceipt(true, "workflow")
	appendReceipt(false, "workflow")
	stored, err = s.PutStepResult(t.Context(), row)
	if err != nil || !stored.CredentialEvidenceKnown || !stored.Credentialed || len(stored.GrantEvidence) != 1 {
		t.Fatal("credentialed admission was declassified", stored, err)
	}
}

func TestNodeReplayCompletenessIncludesCutAndHistoricalAdmissionReceipts(t *testing.T) {
	for _, mode := range []string{"complete", "cut", "missing"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now().UTC()
			s := testStore(t, &now)
			repo := createRepo(t, s)
			run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "completeness", strings.Repeat("b", 40)))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.SetRunWorkflowName(t.Context(), run.ID, "workflow"); err != nil {
				t.Fatal(err)
			}
			receipt := func(complete any) {
				t.Helper()
				fact := map[string]any{"template": "leaf", "known": true, "credentialed": false, "grants": []string{}}
				if complete != nil {
					fact["complete"] = complete
				}
				body, err := json.Marshal(map[string]any{"engine": "argo", "workflow": "workflow", "step_credentials": []any{fact}})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.AppendAuditAction(t.Context(), model.AuditActionSpec{Actor: "actor", Action: "ci.argo.submit.binding", ResourceType: "run", ResourceID: run.ID, Details: string(body)}); err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "cut":
				receipt(false)
			case "missing":
				receipt(nil)
			default:
				receipt(true)
			}
			// A later receipt cannot hide an earlier cut or unknown graph. Group and
			// skipped-only observations must carry the same frozen graph evidence.
			receipt(true)
			for _, nodeType := range []string{"Pod", "DAG", "Skipped"} {
				row, err := s.PutStepResult(t.Context(), model.StepResult{RunID: run.ID, NodeID: nodeType, NodeType: nodeType, Template: "leaf", LocalTemplate: true, Status: model.StepPassed, ReplayEvidenceComplete: true})
				if err != nil || row.ReplayEvidenceComplete != (mode == "complete") {
					t.Fatal("incomplete graph gained replay authority", row, err)
				}
			}
		})
	}
}

func TestNodeUnknownCredentialEvidenceCannotBeDeclassified(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "unknown", strings.Repeat("c", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRunWorkflowName(t.Context(), run.ID, "workflow"); err != nil {
		t.Fatal(err)
	}
	for _, known := range []bool{false, true} {
		body, err := json.Marshal(map[string]any{"engine": "argo", "workflow": "workflow", "step_credentials": []model.StepCredentialEvidence{{Known: known, Complete: true, Template: "leaf", Grants: []string{}}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.AppendAuditAction(t.Context(), model.AuditActionSpec{Actor: "actor", Action: "ci.argo.submit.binding", ResourceType: "run", ResourceID: run.ID, Details: string(body)}); err != nil {
			t.Fatal(err)
		}
	}
	row, err := s.PutStepResult(t.Context(), model.StepResult{RunID: run.ID, NodeID: "node", Template: "leaf", LocalTemplate: true, Status: model.StepPassed})
	if err != nil || !row.ReplayEvidenceComplete || row.CredentialEvidenceKnown {
		t.Fatal("later receipt hid unknown credential evidence", row, err)
	}
}

func TestNativeNodeStepRetainsObservedPodAfterCollection(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "collected", strings.Repeat("d", 40)))
	if err != nil {
		t.Fatal(err)
	}
	step := model.StepResult{RunID: run.ID, NodeID: "node", NodeType: "Pod", Template: "leaf", Phase: "Running", Status: model.StepRunning, PodName: "opaque-pod-name"}
	if _, err := s.PutStepResult(t.Context(), step); err != nil {
		t.Fatal(err)
	}
	step.PodName, step.Phase, step.Status = "", "Succeeded", model.StepPassed
	retained, err := s.PutStepResult(t.Context(), step)
	if err != nil || retained.PodName != "opaque-pod-name" || retained.Phase != "Succeeded" {
		t.Fatal("Pod collection erased observed node history", retained, err)
	}
}
