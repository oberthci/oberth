package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestPruneRunTimingKeepsNewestAndProtectedRuns(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	s, err := Open(ctx, filepath.Join(t.TempDir(), "timing.db"), Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	repo := createRepo(t, s)
	// Two runs on feature/a, one each on feature/b and feature/c, oldest first.
	refs := []string{"feature/a", "feature/a", "feature/b", "feature/c"}
	var runs []model.EnqueueRunResult
	for i, ref := range refs {
		run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, ref, strings.Repeat(fmt.Sprint(i+1), 40)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.ClaimNextRun(ctx); err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("workflow-%d", i)
		if _, err = s.SetRunWorkflowName(ctx, run.ID, name); err != nil {
			t.Fatal(err)
		}
		binding := model.WorkflowTiming{RunID: run.ID, Namespace: "pipelines", Name: name, UID: fmt.Sprintf("11111111-1111-4111-8111-11111111111%d", i), SpecIdentity: strings.Repeat("a", 64), CreatedAt: now, Tasks: []model.TimingTask{{ID: "unit", Template: "unit", Readiness: "all", Dependencies: []string{}}}}
		if err = s.RecordWorkflowTiming(ctx, binding); err != nil {
			t.Fatal(err)
		}
		step := model.StepTiming{Version: 1, RunID: run.ID, Namespace: binding.Namespace, WorkflowName: binding.Name, WorkflowUID: binding.UID, NodeID: "node-1234", PodName: "pod-1234", Stage: "test", Step: "unit", ObservedAt: now.Add(time.Second), ScriptTiming: "mixed", ImagePullTiming: "not-observed", Init: []model.ContainerTiming{}, Missing: []string{"main"}, Unavailable: "observation-unavailable"}
		if err = s.RecordStepTimings(ctx, []model.StepTiming{step}); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
		if _, err = s.FinishRun(ctx, run.ID, model.RunResult{Status: model.RunPassed}); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, run)
	}
	if _, err := s.PruneRunTiming(ctx, 0, 0); err == nil {
		t.Fatal("keepRuns 0 must be refused")
	}
	// Keep the newest terminal run globally and the newest per ref: only the
	// older feature/a run loses its records.
	pruned, err := s.PruneRunTiming(ctx, 1, 1)
	if err != nil || pruned != 1 {
		t.Fatalf("pruned=%d err=%v", pruned, err)
	}
	for i, run := range runs {
		timing, err := s.RunTiming(ctx, run.ID)
		if err != nil {
			t.Fatal(i, err)
		}
		if i == 0 {
			if timing.Unavailable != "pruned" || timing.Workflow != nil || len(timing.Steps) != 0 || timing.ClaimedAt == nil || timing.TerminalAt == nil || timing.WorkflowCreatedAt == nil {
				t.Fatalf("pruned run timing = %+v", timing)
			}
			continue
		}
		if timing.Unavailable != "" || timing.Workflow == nil || len(timing.Steps) != 1 {
			t.Fatalf("run %d timing = %+v", i, timing)
		}
	}
	// Without per-ref protection everything but the newest global run goes, and
	// a second pass finds nothing left.
	if pruned, err = s.PruneRunTiming(ctx, 1, 0); err != nil || pruned != 2 {
		t.Fatalf("pruned=%d err=%v", pruned, err)
	}
	if pruned, err = s.PruneRunTiming(ctx, 1, 0); err != nil || pruned != 0 {
		t.Fatalf("second pass pruned=%d err=%v", pruned, err)
	}
	if timing, err := s.RunTiming(ctx, runs[3].ID); err != nil || timing.Workflow == nil {
		t.Fatalf("newest run timing = %+v err=%v", timing, err)
	}
	if s.DatabaseBytes() <= 0 {
		t.Fatal("database size is unknown")
	}
}
