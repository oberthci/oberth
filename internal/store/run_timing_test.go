package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestRunTimingMilestonesAdmissionEpisodesAndRefire(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/timing", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	queued := now
	now = now.Add(time.Second)
	if _, err = s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	claimed := now
	now = now.Add(time.Second)
	if err = s.RecordRunTimingMilestone(ctx, run.ID, "checkout-ready"); err != nil {
		t.Fatal(err)
	}
	checkout := now
	now = now.Add(time.Second)
	if err = s.RecordAdmissionRequested(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	requested := now
	now = now.Add(time.Second)
	if err = s.RecordAdmissionRequested(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.MarkRunAdmitted(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	granted := now
	now = now.Add(time.Second)
	if _, err = s.MarkRunParked(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	parked := now
	now = now.Add(time.Second)
	if err = s.RecordAdmissionRequested(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	secondRequest := now
	now = now.Add(time.Second)
	if _, err = s.MarkRunAdmitted(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordRunTimingMilestone(ctx, run.ID, "checkout-ready"); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"source-seed-started", "source-ready"} {
		now = now.Add(time.Second)
		if err = s.RecordRunTimingMilestone(ctx, run.ID, phase); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(time.Second)
	refired, err := s.RequeueStrandedRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.RunTiming(ctx, run.ID)
	if err != nil || got.Version != 1 || !got.QueuedAt.Equal(queued) || got.ClaimedAt == nil || !got.ClaimedAt.Equal(claimed) || got.CheckoutReadyAt == nil || !got.CheckoutReadyAt.Equal(checkout) || got.TerminalAt == nil || !got.TerminalAt.Equal(now) || got.SourceReadyAt == nil {
		t.Fatalf("milestones=%+v err=%v", got, err)
	}
	if len(got.Admissions) != 2 || got.Admissions[0].RequestedAt == nil || !got.Admissions[0].RequestedAt.Equal(requested) || got.Admissions[0].GrantedAt == nil || !got.Admissions[0].GrantedAt.Equal(granted) || !got.Admissions[0].ClosedAt.Equal(parked) || !got.Admissions[1].RequestedAt.Equal(secondRequest) || got.Admissions[1].ClosedAt == nil {
		t.Fatalf("episodes=%+v", got.Admissions)
	}
	fresh, err := s.RunTiming(ctx, refired.ID)
	if err != nil || fresh.ClaimedAt != nil || fresh.Workflow != nil || len(fresh.Admissions) != 0 || fresh.Unavailable != "not-recorded" || refired.AdmissionSequence != run.AdmissionSequence {
		t.Fatalf("attempt inherited evidence: %+v %v", fresh, err)
	}
	for _, query := range []string{`UPDATE run_timings SET claimed_at=1 WHERE run_id=?`, `UPDATE run_admission_timings SET requested_at=1 WHERE run_id=?`} {
		if _, err = s.db.Exec(query, run.ID); err == nil {
			t.Fatal("immutable milestone rewritten")
		}
	}
}

func TestRunTimingWorkflowAndPodAttemptSurviveReopen(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "timing.db")
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/timing", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetRunJobName(ctx, run.ID, "workflow-test"); err != nil {
		t.Fatal(err)
	}
	binding := model.WorkflowTiming{RunID: run.ID, Namespace: "pipelines", Name: "workflow-test", UID: "11111111-1111-4111-8111-111111111111", SpecIdentity: strings.Repeat("a", 64), CreatedAt: now, Tasks: []model.TimingTask{{ID: "unit", Template: "unit", Readiness: "all", Dependencies: []string{}}}}
	if err = s.RecordWorkflowTiming(ctx, binding); err != nil {
		t.Fatal(err)
	}
	main := &model.ContainerTiming{Name: "main", ContainerID: "containerd://" + strings.Repeat("c", 64), StartedAt: now, FinishedAt: now.Add(time.Second)}
	value := model.StepTiming{Version: 1, RunID: run.ID, Namespace: binding.Namespace, WorkflowName: binding.Name, WorkflowUID: binding.UID, NodeID: "node-1234", PodName: "pod-1234", Burn: "test", Step: "unit", ObservedAt: now.Add(2 * time.Second), ScriptTiming: "mixed", ImagePullTiming: "not-observed", Init: []model.ContainerTiming{}, Missing: []string{"main"}, Unavailable: "observation-unavailable"}
	if err = s.RecordStepTimings(ctx, []model.StepTiming{value}); err != nil {
		t.Fatal(err)
	}
	// Even valid-looking JSON must match the protected SQL keys and retained
	// Workflow binding. Never serve evidence from a corrupted/mixed attempt.
	originalJSON, _ := json.Marshal(value)
	for _, field := range []string{"workflow", "node", "pod"} {
		wrong := value
		switch field {
		case "workflow":
			wrong.WorkflowUID = "33333333-3333-4333-8333-333333333333"
		case "node":
			wrong.NodeID = "other-node"
		case "pod":
			wrong.PodUID = "33333333-3333-4333-8333-333333333333"
		}
		raw, _ := json.Marshal(wrong)
		if _, err = s.db.Exec(`UPDATE step_timings SET timing_json=? WHERE run_id=?`, string(raw), run.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = s.RunTiming(ctx, run.ID); !errors.Is(err, ErrInvalidState) {
			t.Fatal("mixed timing keys served", field, err)
		}
	}
	if _, err = s.db.Exec(`UPDATE step_timings SET timing_json=? WHERE run_id=?`, string(originalJSON), run.ID); err != nil {
		t.Fatal(err)
	}

	value.Available = true
	value.Unavailable = ""
	value.PodUID = "22222222-2222-4222-8222-222222222222"
	value.Main = main
	value.Missing = []string{}
	if err = s.RecordStepTimings(ctx, []model.StepTiming{value}); err != nil {
		t.Fatal(err)
	}
	value.ObservedAt = value.ObservedAt.Add(time.Second)
	if err = s.RecordStepTimings(ctx, []model.StepTiming{value}); err != nil {
		t.Fatal("idempotent observation", err)
	}
	for _, mode := range []string{"pod", "workflow", "container", "ordinal", "step", "time"} {
		t.Run(mode, func(t *testing.T) {
			changed := value
			copyMain := *main
			changed.Main = &copyMain
			switch mode {
			case "pod":
				changed.PodUID = "33333333-3333-4333-8333-333333333333"
			case "workflow":
				changed.WorkflowUID = "33333333-3333-4333-8333-333333333333"
			case "container":
				changed.Main.ContainerID = "containerd://" + strings.Repeat("d", 64)
			case "ordinal":
				changed.Ordinal++
			case "step":
				changed.Step = "other"
			case "time":
				changed.Main.FinishedAt = now
			}
			if err = s.RecordStepTimings(ctx, []model.StepTiming{changed}); !errors.Is(err, ErrInvalidState) {
				t.Fatalf("changed attempt accepted: %v", err)
			}
		})
	}
	changed := binding
	changed.UID = "33333333-3333-4333-8333-333333333333"
	if err = s.RecordWorkflowTiming(ctx, changed); !errors.Is(err, ErrInvalidState) {
		t.Fatal("Workflow rebound", err)
	}
	if _, err = s.db.Exec(`UPDATE step_timings SET pod_uid='other' WHERE run_id=?`, run.ID); err == nil {
		t.Fatal("SQL attempt rebound")
	}
	now = now.Add(time.Minute)
	if _, err = s.FinishRun(ctx, run.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}
	before, err := s.RunTiming(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	// No Workflow or Pod client is involved: all facts survive TTL and restart.
	s, err = Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	after, err := s.RunTiming(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if string(a) != string(b) || len(after.Steps) != 1 || after.Workflow == nil || after.Steps[0].Main == nil || after.TerminalAt == nil {
		t.Fatalf("retention changed: %s / %s", a, b)
	}
}

func TestRunTimingDoesNotInferAdmissionFromLegacySubmission(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	ctx := context.Background()
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "legacy", strings.Repeat("c", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetRunJobName(ctx, run.ID, "workflow-legacy"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.MarkRunSubmitted(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.RunTiming(ctx, run.ID)
	if err != nil || got.ClaimedAt == nil || len(got.Admissions) != 0 || got.WorkflowCreatedAt != nil {
		t.Fatalf("inferred overloaded timestamps: %+v %v", got, err)
	}
}

func TestRunTimingMigrationDoesNotBackfillLegacyStartedAt(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(100, 0)
	path := filepath.Join(t.TempDir(), "pre-timing.db")
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "legacy", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetRunJobName(ctx, run.ID, "workflow-legacy"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.MarkRunSubmitted(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishRun(ctx, run.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`ALTER TABLE schedule_fires DROP COLUMN last_fired_at; DELETE FROM schema_migrations WHERE version=32; DROP TRIGGER run_schedule_entry_immutable; ALTER TABLE runs DROP COLUMN schedule_entry; DELETE FROM schema_migrations WHERE version=31; DROP TRIGGER run_timing_claim; DROP TRIGGER run_timing_grant; DROP TRIGGER run_timing_park; DROP TRIGGER run_timing_terminal; DROP TABLE step_timings; DROP TABLE run_admission_timings; DROP TABLE run_timings; DELETE FROM schema_migrations WHERE version=?`, runTimingMigrationVersion); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	s, err = Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.RunTiming(ctx, run.ID)
	if err != nil || got.Unavailable != "not-recorded" || got.ClaimedAt != nil || got.TerminalAt != nil || len(got.Admissions) != 0 {
		t.Fatalf("legacy inference: %+v %v", got, err)
	}
	retained, err := s.Run(ctx, run.ID)
	if err != nil || retained.Status != model.RunPassed || retained.StartedAt == nil || retained.StartedAt.Equal(now) {
		t.Fatalf("legacy run changed: %+v %v", retained, err)
	}
	for version := 1; version < runTimingMigrationVersion; version++ {
		var at int64
		if err = s.db.QueryRow(`SELECT applied_at FROM schema_migrations WHERE version=?`, version).Scan(&at); err != nil || at != unixNano(time.Unix(100, 0)) {
			t.Fatalf("migration %d replayed: %d %v", version, at, err)
		}
	}
	assertFreshSchemaLedger(t, s.db)
}

func TestRunTimingNewRequestOnLegacyActiveRunKeepsEarlierPhasesUnknown(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	ctx := context.Background()
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "legacy-active", strings.Repeat("d", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	// Emulate an active row claimed before the timing migration. The new
	// request is knowable; the earlier claim/grant timestamps are not.
	if _, err = s.db.Exec(`DELETE FROM run_timings WHERE run_id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if err = s.RecordAdmissionRequested(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.RunTiming(ctx, run.ID)
	if err != nil || got.Unavailable != "" || got.ClaimedAt != nil || len(got.Admissions) != 1 || got.Admissions[0].RequestedAt == nil || !got.Admissions[0].RequestedAt.Equal(now) || got.Admissions[0].GrantedAt != nil {
		t.Fatalf("partial legacy observation lost/inferred: %+v %v", got, err)
	}
}
