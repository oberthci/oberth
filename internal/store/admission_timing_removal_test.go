package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestAdmissionTimingRemovalPreservesWorkflowAndStepHistory(t *testing.T) {
	s := newMigrationIDTestStore(t)
	ctx := t.Context()
	index := -1
	for i, migration := range migrations {
		if strings.Contains(migration.id, "-remove-admission-timing-") {
			index = i
			break
		}
	}
	if index < 0 {
		t.Fatal("admission timing migration missing")
	}
	if err := s.migrateWithMigrations(ctx, nil, migrations[:index]); err != nil {
		t.Fatal(err)
	}
	if err := s.configureLiveReaders(ctx, 0); err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	run := projectionRun(t, s, repo.ID, "feature/timing-removal", projectionSourceSHA)
	if _, err := s.SetRunWorkflowName(ctx, run.ID, "workflow-timing"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	binding := model.WorkflowTiming{RunID: run.ID, Namespace: "pipelines", Name: "workflow-timing", UID: "11111111-1111-4111-8111-111111111111", SpecIdentity: strings.Repeat("a", 64), CreatedAt: now, Tasks: []model.TimingTask{{ID: "unit", Template: "unit", Readiness: "all", Dependencies: []string{}}}}
	if err := s.RecordWorkflowTiming(ctx, binding); err != nil {
		t.Fatal(err)
	}
	step := model.StepTiming{Version: 1, RunID: run.ID, Namespace: binding.Namespace, WorkflowName: binding.Name, WorkflowUID: binding.UID, NodeID: "node-1234", PodName: "pod-1234", Stage: "test", Step: "unit", ObservedAt: now.Add(time.Second), ScriptTiming: "mixed", ImagePullTiming: "not-observed", Init: []model.ContainerTiming{}, Missing: []string{"main"}, Unavailable: "observation-unavailable"}
	if err := s.RecordStepTimings(ctx, []model.StepTiming{step}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO run_admission_timings(run_id,episode,requested_at,granted_at) VALUES(?,1,1,2)`, run.ID); err != nil {
		t.Fatal(err)
	}
	before, err := s.RunTiming(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.migrateWithMigrations(ctx, nil, migrations); err != nil {
		t.Fatal(err)
	}
	after, err := s.RunTiming(ctx, run.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("durable timing changed: before=%+v after=%+v err=%v", before, after, err)
	}
	for _, name := range []string{"run_admission_timings", "admission_timing_immutable", "run_timing_grant", "run_timing_park", "run_admission_timing_open"} {
		var count int
		if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name=?`, name).Scan(&count); err != nil || count != 0 {
			t.Fatalf("admission object %s remains: %d %v", name, count, err)
		}
	}
	// Both submission and terminal transitions must work after removing their old admission-trigger writes.
	if _, err := s.MarkRunSubmissionStarted(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishRun(ctx, run.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}
	after, err = s.RunTiming(ctx, run.ID)
	if err != nil || after.TerminalAt == nil || after.Workflow == nil || len(after.Steps) != 1 {
		t.Fatalf("history missing after completion: %+v %v", after, err)
	}
	body, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"admissions"`, `"requested_at"`, `"granted_at"`, `"closed_at"`} {
		if strings.Contains(string(body), field) {
			t.Fatalf("removed field %s in timing", field)
		}
	}
}
