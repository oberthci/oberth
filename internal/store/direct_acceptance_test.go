package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestDirectAcceptanceStartsInTheReceiveTransactionAndRecoversOnRestart(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	spec := testRunSpec(repo.ID, "feature/atomic", strings.Repeat("a", 40))
	event := model.ReceiveEventSpec{ID: "atomic-accept", RepoID: repo.ID, Actor: spec.Actor, RefKind: spec.RefKind, Ref: spec.Ref, ObjectSHA: spec.SHA, CommitSHA: spec.SHA, Outcome: "queued"}
	accepted, err := s.EnqueueReceiveEvent(t.Context(), event, spec)
	if err != nil || accepted.Status != model.RunRunning || accepted.Phase != "waiting" {
		t.Fatalf("acceptance was not started atomically: %+v %v", accepted, err)
	}
	if err := s.RecoverOwnerState(t.Context()); err != nil {
		t.Fatal(err)
	}
	replay, err := s.EnqueueReceiveEvent(t.Context(), event, spec)
	if err != nil || !replay.Duplicate || replay.Status != model.RunFailed || replay.Error != model.ServerRestartReason {
		t.Fatalf("interrupted acceptance replay=%+v %v", replay, err)
	}
}

func TestDirectAcceptanceRecoversOldQueuedRowsAndTheirReceiveReplay(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	spec := testRunSpec(repo.ID, "feature/prior", strings.Repeat("b", 40))
	event := model.ReceiveEventSpec{ID: "prior-accept", RepoID: repo.ID, Actor: spec.Actor, RefKind: spec.RefKind, Ref: spec.Ref, ObjectSHA: spec.SHA, CommitSHA: spec.SHA, Outcome: "queued"}
	accepted, err := s.EnqueueReceiveEvent(t.Context(), event, spec)
	if err != nil {
		t.Fatal(err)
	}
	// Old releases committed this shape before their dispatch loop claimed it.
	if _, err := s.db.ExecContext(t.Context(), `UPDATE runs SET status='queued',phase='queued',started_at=NULL WHERE id=?`, accepted.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverOwnerState(t.Context()); err != nil {
		t.Fatal(err)
	}
	replay, err := s.EnqueueReceiveEvent(t.Context(), event, spec)
	if err != nil || !replay.Duplicate || replay.Status != model.RunFailed || replay.Error != model.ServerRestartReason {
		t.Fatalf("prior receive stranded=%+v %v", replay, err)
	}
}

func TestDirectAcceptanceRestartRearmsFailedAndCancelledCreates(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	want := map[string]bool{}
	for _, kind := range []string{"failed-create", "cancelled-create", "test-failure"} {
		accepted, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "feature/"+kind, strings.Repeat("a", 40)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetRunWorkflowName(t.Context(), accepted.ID, "workflow-"+kind); err != nil {
			t.Fatal(err)
		}
		result := model.RunResult{Status: model.RunFailed, Phase: "infrastructure", Error: "create Workflow: outcome unknown"}
		if kind == "cancelled-create" {
			result = model.RunResult{Status: model.RunInterrupted, Phase: model.PhaseCancelled, Error: "cancelled by owner"}
		}
		if kind == "test-failure" {
			result = model.RunResult{Status: model.RunFailed, Phase: "job", Error: "unit failed"}
		}
		if _, err := s.FinishRun(t.Context(), accepted.ID, result); err != nil {
			t.Fatal(err)
		}
		if kind != "test-failure" {
			want[accepted.ID] = true
		}
	}
	runs, err := s.FailedWorkflowCreates(t.Context())
	if err != nil || len(runs) != len(want) {
		t.Fatal(runs, err)
	}
	for _, run := range runs {
		if !want[run.ID] {
			t.Fatalf("normal test failure selected for shutdown: %+v", run)
		}
	}
}
