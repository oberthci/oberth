package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestRestartRequeuesUnexecutedRunUnderSameID(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    model.RefKind
		release bool
	}{{"branch", model.RefBranch, false}, {"tag", model.RefTag, false}, {"release", model.RefTag, true}} {
		t.Run(tc.name, func(t *testing.T) {
			kind := tc.kind
			now := time.Now().UTC()
			s := testStore(t, &now)
			repo := createRepo(t, s)
			spec := testRunSpec(repo.ID, "feature/restart", strings.Repeat("a", 40))
			spec.RefKind, spec.Release = kind, tc.release
			if kind == model.RefTag {
				spec.Ref = "v1.2.3"
				spec.Trigger = "tag"
			}
			original, err := s.EnqueueRun(t.Context(), spec)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.ClaimNextRun(t.Context()); err != nil {
				t.Fatal(err)
			}
			later, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "feature/later", strings.Repeat("b", 40)))
			if err != nil {
				t.Fatal(err)
			}
			reset, err := s.RequeueStrandedRun(t.Context(), original.ID)
			if err != nil {
				t.Fatal(err)
			}
			if reset.ID != original.ID || reset.QueueSequence != original.QueueSequence || reset.AdmissionSequence != original.AdmissionSequence || reset.Status != model.RunQueued {
				t.Fatalf("restart lost durable identity/order: original=%+v reset=%+v", original.Run, reset)
			}
			claimed, err := s.ClaimNextRun(t.Context())
			if err != nil || claimed.ID != original.ID {
				t.Fatalf("later run %s overtook restart: %s %v", later.ID, claimed.ID, err)
			}
		})
	}
}

func TestRestartResetWaitsForOldWorkflowCancellation(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	original, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "feature/cancel", strings.Repeat("c", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetRunWorkflowName(t.Context(), original.ID, "old-workflow"); err != nil {
		t.Fatal(err)
	}
	reset, err := s.RequeueStrandedRun(t.Context(), original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reset.ID != original.ID {
		t.Fatal("reset replaced the run ID")
	}
	if next, err := s.ClaimNextRun(t.Context()); err == nil {
		t.Fatalf("claimed before old Workflow cleanup: %s", next.ID)
	}
	pending, err := s.PendingRunCancellations(t.Context())
	if err != nil || len(pending) != 1 || pending[0].WorkflowName != "old-workflow" {
		t.Fatalf("missing cleanup obligation: %v %v", pending, err)
	}
	if err = s.CompleteObservedRunCancellation(t.Context(), pending[0]); err != nil {
		t.Fatal(err)
	}
	next, err := s.ClaimNextRun(t.Context())
	if err != nil || next.ID != original.ID {
		t.Fatalf("cleanup did not unblock same run: %+v %v", next, err)
	}
}

func TestRestartResetRefusesExecutionAndPublicationEvidence(t *testing.T) {
	for _, evidence := range []string{"step", "publication"} {
		t.Run(evidence, func(t *testing.T) {
			now := time.Now().UTC()
			s := testStore(t, &now)
			repo := createRepo(t, s)
			run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "evidence", strings.Repeat("d", 40)))
			requireRestartNoError(t, err)
			_, err = s.ClaimNextRun(t.Context())
			requireRestartNoError(t, err)
			if evidence == "step" {
				_, err = s.PutStepResult(t.Context(), model.StepResult{RunID: run.ID, Stage: "test", Step: "unit", Status: model.StepPassed, DeclaredSize: "S"})
			} else {
				_, err = s.BeginPublication(t.Context(), model.PublicationSpec{RepoID: repo.ID, RunID: run.ID, RefKind: run.RefKind, Ref: run.Ref, ResultSHA: run.SHA, Actor: run.Actor})
			}
			requireRestartNoError(t, err)
			if _, err = s.ResetStrandedRunToQueued(t.Context(), run.ID); !errors.Is(err, ErrRequeueIneligible) {
				t.Fatalf("reset with %s: %v", evidence, err)
			}
			current, err := s.Run(t.Context(), run.ID)
			requireRestartNoError(t, err)
			if current.Status != model.RunRunning {
				t.Fatal(current)
			}
		})
	}
}

func TestRestartRecordsNewWorkflowTimingWithoutRewritingHistory(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "timing-restart", strings.Repeat("e", 40)))
	requireRestartNoError(t, err)
	_, err = s.ClaimNextRun(t.Context())
	requireRestartNoError(t, err)
	_, err = s.SetRunWorkflowName(t.Context(), run.ID, "workflow-before")
	requireRestartNoError(t, err)
	first := model.WorkflowTiming{RunID: run.ID, Namespace: "pipelines", Name: "workflow-before", UID: "11111111-1111-4111-8111-111111111111", SpecIdentity: strings.Repeat("a", 64), CreatedAt: now, Tasks: []model.TimingTask{}}
	requireRestartNoError(t, s.RecordWorkflowTiming(t.Context(), first))
	_, err = s.ResetStrandedRunToQueued(t.Context(), run.ID)
	requireRestartNoError(t, err)
	if _, err = s.RunTiming(t.Context(), run.ID); err != nil {
		t.Fatal("queued timing unreadable", err)
	}
	pending, err := s.PendingRunCancellations(t.Context())
	requireRestartNoError(t, err)
	requireRestartNoError(t, s.CompleteObservedRunCancellation(t.Context(), pending[0]))
	_, err = s.ClaimNextRun(t.Context())
	requireRestartNoError(t, err)
	_, err = s.SetRunWorkflowName(t.Context(), run.ID, "workflow-after")
	requireRestartNoError(t, err)
	second := first
	second.Name = "workflow-after"
	second.UID = "22222222-2222-4222-8222-222222222222"
	second.CreatedAt = now.Add(time.Minute)
	requireRestartNoError(t, s.RecordWorkflowTiming(t.Context(), second))
	got, err := s.RunTiming(t.Context(), run.ID)
	requireRestartNoError(t, err)
	if got.Workflow == nil || got.Workflow.UID != second.UID {
		t.Fatal(got)
	}
	second.UID = "33333333-3333-4333-8333-333333333333"
	if err = s.RecordWorkflowTiming(t.Context(), second); !errors.Is(err, ErrInvalidState) {
		t.Fatal("rewrote same-attempt binding", err)
	}
	var legacy string
	requireRestartNoError(t, s.db.QueryRow(`SELECT workflow_json FROM run_timings WHERE run_id=?`, run.ID).Scan(&legacy))
	if !strings.Contains(legacy, first.UID) {
		t.Fatal("lost prior timing", legacy)
	}
}

func TestTagPublicationUsesRepositoryTagAndSHATuple(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), model.RunSpec{RepoID: repo.ID, RefKind: model.RefTag, Ref: "v1.2.3", SHA: strings.Repeat("a", 40), Actor: "release", Trigger: "tag", Release: true})
	requireRestartNoError(t, err)
	_, err = s.ClaimNextRun(t.Context())
	requireRestartNoError(t, err)
	spec := model.PublicationSpec{RepoID: repo.ID, RunID: run.ID, RefKind: model.RefTag, Ref: run.Ref, ResultSHA: run.SHA, Actor: run.Actor}
	first, err := s.BeginPublication(t.Context(), spec)
	requireRestartNoError(t, err)
	again, err := s.BeginPublication(t.Context(), spec)
	if err != nil || again.ID != first.ID {
		t.Fatalf("same release lost its outbox intent: %+v %v", again, err)
	}
	_, err = s.FinalizePublication(t.Context(), first.ID, model.PublicationDelivered, "")
	requireRestartNoError(t, err)
	again, err = s.BeginPublication(t.Context(), spec)
	if err != nil || again.ID != first.ID || again.Status != model.PublicationDelivered {
		t.Fatalf("delivered release lost its identity: %+v %v", again, err)
	}
	duplicate, err := s.EnqueueRun(t.Context(), model.RunSpec{RepoID: repo.ID, RefKind: model.RefTag, Ref: run.Ref, SHA: run.SHA, Actor: run.Actor, Trigger: "tag", Release: true})
	requireRestartNoError(t, err)
	_, err = s.ClaimNextRun(t.Context())
	requireRestartNoError(t, err)
	spec.RunID = duplicate.ID
	if _, err = s.BeginPublication(t.Context(), spec); !errors.Is(err, ErrInvalidState) {
		t.Fatal("duplicate release acquired another publication", err)
	}
	if _, err = s.ResetStrandedRunToQueued(t.Context(), duplicate.ID); !errors.Is(err, ErrRequeueIneligible) {
		t.Fatal("restart repeated already-published release", err)
	}
}

func requireRestartNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestRestartMigrationsUpgradeV38WithoutLosingReleaseIntent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v38.db")
	s, err := Open(context.Background(), path, Options{})
	requireRestartNoError(t, err)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), model.RunSpec{RepoID: repo.ID, RefKind: model.RefTag, Ref: "v2.0.0", SHA: strings.Repeat("a", 40), Actor: "release", Trigger: "tag", Release: true})
	requireRestartNoError(t, err)
	_, err = s.ClaimNextRun(t.Context())
	requireRestartNoError(t, err)
	publication, err := s.BeginPublication(t.Context(), model.PublicationSpec{RepoID: repo.ID, RunID: run.ID, RefKind: model.RefTag, Ref: run.Ref, ResultSHA: run.SHA, Actor: run.Actor})
	requireRestartNoError(t, err)
	_, err = s.db.Exec(integrationDowngradeSQL)
	requireRestartNoError(t, err)
	requireRestartNoError(t, s.Close())
	upgraded, err := Open(context.Background(), path, Options{})
	requireRestartNoError(t, err)
	defer func() { requireRestartNoError(t, upgraded.Close()) }()
	assertFreshSchemaLedger(t, upgraded.db)
	current, err := upgraded.Run(t.Context(), run.ID)
	requireRestartNoError(t, err)
	if current.QueueSequence != run.QueueSequence || current.AdmissionSequence != run.AdmissionSequence || !current.Release {
		t.Fatal("upgrade changed release identity", current)
	}
	pending, err := upgraded.PendingPublications(t.Context())
	requireRestartNoError(t, err)
	if len(pending) != 1 || pending[0].ID != publication.ID || pending[0].RunID != run.ID {
		t.Fatalf("upgrade changed outbox: %+v", pending)
	}
	_, err = upgraded.ResetStrandedRunToQueued(t.Context(), run.ID)
	if !errors.Is(err, ErrRequeueIneligible) {
		t.Fatal("publication owner reset after upgrade", err)
	}
}
