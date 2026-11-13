package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/testdb"
)

const removeRestartTimingsMigrationID = "20261113181825-remove-restart-timings-370d05c58486149250a7139eb6afdca7"

// Only upgrade fixtures reconstruct the retired schema; runtime timing has one
// binding per run and never interprets historical restart audit counts.
func historicalRestartTimingSQL(t *testing.T) string {
	t.Helper()
	for _, migration := range legacyMigrations {
		if migration.version == 44 {
			return migration.sql
		}
	}
	t.Fatal("missing frozen v44 migration")
	return ""
}

func TestRemoveRestartTimingsPreservesCurrentDurableEvidence(t *testing.T) {
	for _, history := range []string{"ordinary", "current-attempt", "missing-current-attempt"} {
		t.Run(history, func(t *testing.T) {
			ctx := t.Context()
			now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
			s := testStore(t, &now)
			repo := createRepo(t, s)
			run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/timing", strings.Repeat("a", 40)))
			requireRestartNoError(t, err)
			_, err = s.ClaimNextRun(ctx)
			requireRestartNoError(t, err)
			_, err = s.SetRunWorkflowName(ctx, run.ID, "workflow-before")
			requireRestartNoError(t, err)
			requireRestartNoError(t, s.RecordRunTimingMilestone(ctx, run.ID, "source-ready"))
			workflow := model.WorkflowTiming{RunID: run.ID, Namespace: "pipelines", Name: "workflow-before", UID: "11111111-1111-4111-8111-111111111111", SpecIdentity: strings.Repeat("a", 64), CreatedAt: now, Tasks: []model.TimingTask{}}
			step := model.StepTiming{Version: 1, RunID: run.ID, Namespace: workflow.Namespace, WorkflowName: workflow.Name, WorkflowUID: workflow.UID, NodeID: "node-before", Ordinal: 0, Stage: "test", Step: "unit", ObservedAt: now, ScriptTiming: "mixed", ImagePullTiming: "not-observed", Init: []model.ContainerTiming{}, Missing: []string{"main"}, Unavailable: "observation-unavailable"}
			requireRestartNoError(t, s.RecordWorkflowTiming(ctx, workflow))
			requireRestartNoError(t, s.RecordStepTimings(ctx, []model.StepTiming{step}))
			before, err := s.RunTiming(ctx, run.ID)
			requireRestartNoError(t, err)
			_, err = s.db.Exec(historicalRestartTimingSQL(t))
			requireRestartNoError(t, err)
			appendHistoricalRestart := func() {
				t.Helper()
				_, err := s.AppendAuditAction(ctx, model.AuditActionSpec{Actor: "historical-owner", Action: "run.restart", ResourceType: "run", ResourceID: run.ID, Details: `{}`})
				requireRestartNoError(t, err)
			}
			if history != "ordinary" {
				appendHistoricalRestart()
				workflow.Name, workflow.UID = "workflow-after", "22222222-2222-4222-8222-222222222222"
				workflow.CreatedAt = now.Add(time.Minute)
				step.WorkflowName, step.WorkflowUID, step.NodeID = workflow.Name, workflow.UID, "node-after"
				body, err := json.Marshal(workflow)
				requireRestartNoError(t, err)
				_, err = s.db.Exec(`INSERT INTO restart_workflow_timings(run_id,epoch,workflow_created_at,workflow_json) VALUES(?,1,?,?)`, run.ID, unixNano(workflow.CreatedAt), string(body))
				requireRestartNoError(t, err)
				body, err = json.Marshal(step)
				requireRestartNoError(t, err)
				_, err = s.db.Exec(`INSERT INTO restart_step_timings(run_id,epoch,node_id,workflow_uid,pod_uid,available,ordinal,timing_json) VALUES(?,1,?,?,'',0,0,?)`, run.ID, step.NodeID, step.WorkflowUID, string(body))
				requireRestartNoError(t, err)
				_, err = s.db.Exec(`UPDATE runs SET job_name=? WHERE id=?`, workflow.Name, run.ID)
				requireRestartNoError(t, err)
				if history == "missing-current-attempt" {
					appendHistoricalRestart()
				}
			}
			_, err = s.db.Exec(testdb.PostWorkflowStopDowngrade+`-- Reconstruct the old column because this fixture rewinds past its drop migration.
ALTER TABLE runs ADD COLUMN credentialed INTEGER NOT NULL DEFAULT 0 CHECK (credentialed IN (0, 1));
DELETE FROM schema_migrations WHERE version >= (SELECT version FROM schema_migrations WHERE id=?)`, removeRestartTimingsMigrationID)
			requireRestartNoError(t, err)
			requireRestartNoError(t, s.migrate(ctx, nil))
			var retired int
			requireRestartNoError(t, s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name IN ('restart_step_timings','restart_workflow_timings','restart_step_timing_immutable','restart_workflow_timing_immutable')`).Scan(&retired))
			if retired != 0 {
				t.Fatal("retired timing schema survived", retired)
			}
			after, err := s.RunTiming(ctx, run.ID)
			requireRestartNoError(t, err)
			if !reflect.DeepEqual(before.ClaimedAt, after.ClaimedAt) || !reflect.DeepEqual(before.SourceReadyAt, after.SourceReadyAt) {
				t.Fatal("migration lost durable milestones", before, after)
			}
			switch history {
			case "ordinary":
				if !reflect.DeepEqual(before, after) {
					t.Fatal("ordinary timing changed", before, after)
				}
			case "current-attempt":
				if !reflect.DeepEqual(after.Workflow, &workflow) || len(after.Steps) != 1 || !reflect.DeepEqual(after.Steps[0], step) || after.WorkflowCreatedAt == nil || !after.WorkflowCreatedAt.Equal(workflow.CreatedAt) {
					t.Fatal("current attempt evidence lost or mixed", after)
				}
			case "missing-current-attempt":
				if after.Workflow != nil || after.WorkflowCreatedAt != nil || len(after.Steps) != 0 {
					t.Fatal("stale attempt evidence served", after)
				}
			}
			// The ordinary writer still accepts the retained binding (or its first
			// observation when the current attempt had none) after the drop.
			requireRestartNoError(t, s.RecordWorkflowTiming(ctx, workflow))
			requireRestartNoError(t, s.RecordStepTimings(ctx, []model.StepTiming{step}))
			stable, err := s.RunTiming(ctx, run.ID)
			requireRestartNoError(t, err)
			appendHistoricalRestart()
			got, err := s.RunTiming(ctx, run.ID)
			requireRestartNoError(t, err)
			if !reflect.DeepEqual(stable, got) {
				t.Fatal("runtime timing still selects audit epochs", stable, got)
			}
		})
	}
}
