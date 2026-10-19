package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestStepDiagnosticMigrationAndDurableRoundTrip(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "legacy20.db")
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/diagnostic", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	step := model.StepResult{RunID: run.ID, Burn: "test", Step: "unit", Status: model.StepFailed, ExitCode: 137, DeclaredSize: "M"}
	if _, err = s.PutStepResult(ctx, step); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`DROP TABLE maintenance_windows; DROP TABLE kubevirt_grants; DROP TABLE uplink_scopes; ALTER TABLE issues DROP COLUMN scope_upstream_id; ALTER TABLE step_results DROP COLUMN termination_json; DELETE FROM schema_migrations WHERE version>=21;`); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.StepResults(ctx, run.ID)
	if err != nil || len(got) != 1 || got[0].Status != model.StepFailed || got[0].ExitCode != 137 || got[0].Termination.Unavailable != "not-recorded" {
		t.Fatalf("legacy preservation %#v %v", got, err)
	}
	step.Termination = &model.StepDiagnostic{Available: true, ObservedAt: &now, Container: &model.StepTermination{RunID: run.ID, Namespace: "pipelines", WorkflowName: "workflow-test", WorkflowUID: "11111111-1111-4111-8111-111111111111", NodeID: "workflow-test-1234", PodName: "workflow-test-main-1234", PodUID: "22222222-2222-4222-8222-222222222222", ContainerName: "main", ContainerID: "containerd://" + strings.Repeat("c", 64), Reason: "OOMKilled", PodReason: "none", ExitCode: 137, StartedAt: now.Add(-time.Minute), FinishedAt: now.Add(-time.Second)}}
	if _, err = s.PutStepResult(ctx, step); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	got, err = s.StepResults(ctx, run.ID)
	if err != nil || !got[0].Termination.Available || got[0].Termination.Container.Reason != "OOMKilled" || got[0].ExitCode != 137 {
		t.Fatalf("durable observation %#v %v", got, err)
	}
	raw, _ := json.Marshal(got[0].Termination)
	for _, bad := range []string{strings.Replace(string(raw), "OOMKilled", "SECRET-CANARY", 1), `{"available":false,"unavailable":"not-recorded","available":false}`, `{"available":false,"unavailable":"not-recorded","message":"SECRET-CANARY"}`, strings.Repeat("x", 4097), string(raw) + `{}`, strings.Replace(string(raw), run.ID, strings.Repeat("f", 32), 1)} {
		d := readStepDiagnostic(sql.NullString{String: bad, Valid: true}, run.ID)
		b, _ := json.Marshal(d)
		if d.Available || d.Unavailable != "invalid-termination" || strings.Contains(string(b), "SECRET-CANARY") {
			t.Fatal("malformed diagnostic escaped")
		}
	}
	// Optional bad evidence is downgraded without losing the original failed row.
	step.Termination.Container.RunID = strings.Repeat("f", 32)
	gotStep, err := s.PutStepResult(ctx, step)
	if err != nil || gotStep.Status != model.StepFailed || gotStep.ExitCode != 137 || gotStep.Termination.Unavailable != "invalid-termination" {
		t.Fatal("bad optional evidence masked verdict")
	}
}
