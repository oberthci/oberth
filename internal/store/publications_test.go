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

func TestPublicationIntentSurvivesRestartAndFinalizesRunAtomically(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "oberth.db")
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/durable", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	publication, err := s.BeginPublication(ctx, model.PublicationSpec{
		RepoID: repo.ID, RunID: run.ID, RefKind: model.RefBranch, Ref: run.Ref,
		PreviousSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ResultSHA: run.SHA,
		Actor: run.Actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if publication.Status != model.PublicationPending {
		t.Fatalf("publication status = %q, want pending", publication.Status)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	now = now.Add(time.Minute)
	s, err = Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	recovered, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != model.RunRunning || recovered.Phase != "publishing" || recovered.FinishedAt != nil {
		t.Fatalf("publication-owned run after restart = %#v", recovered)
	}
	pending, err := s.PendingPublications(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != publication.ID {
		t.Fatalf("pending publications = %#v, %v", pending, err)
	}

	finalized, err := s.FinalizePublication(ctx, publication.ID, model.PublicationDelivered, "")
	if err != nil {
		t.Fatal(err)
	}
	if finalized.Publication.Status != model.PublicationDelivered || finalized.Run.Status != model.RunPassed ||
		finalized.Run.FinishedAt == nil || finalized.Promotion.ID != "" {
		t.Fatalf("delivered publication finalization = %#v", finalized)
	}
	if _, err := s.FinalizePublication(ctx, publication.ID, model.PublicationFailed, "late failure"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("second publication finalization error = %v, want ErrInvalidState", err)
	}
}

func TestPublicationBindsObservedPredecessorAfterDurableIntent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/predecessor", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	publication, err := s.BeginPublication(ctx, model.PublicationSpec{
		RepoID: repo.ID, RunID: run.ID, RefKind: model.RefBranch, Ref: run.Ref,
		ResultSHA: run.SHA, Actor: run.Actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if publication.PreviousKnown || publication.PreviousSHA != "" {
		t.Fatalf("new publication predecessor = %#v, want unknown", publication)
	}
	const predecessor = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	bound, err := s.SetPublicationPredecessor(ctx, publication.ID, predecessor, true)
	if err != nil {
		t.Fatal(err)
	}
	if !bound.PreviousKnown || bound.PreviousSHA != predecessor {
		t.Fatalf("bound publication predecessor = %#v", bound)
	}
	if _, err := s.SetPublicationPredecessor(ctx, publication.ID, predecessor, true); err != nil {
		t.Fatalf("idempotent predecessor bind: %v", err)
	}
	if _, err := s.SetPublicationPredecessor(ctx, publication.ID, "cccccccccccccccccccccccccccccccccccccccc", true); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("changed predecessor error = %v, want ErrInvalidState", err)
	}
}

func TestAuditStateRejectsPendingPublicationRowChangedAfterIntent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/audit-intent", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	publication, err := s.BeginPublication(ctx, model.PublicationSpec{
		RepoID: repo.ID, RunID: run.ID, RefKind: model.RefBranch, Ref: run.Ref,
		PreviousSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ResultSHA: run.SHA, Actor: run.Actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyAuditState(ctx); err != nil {
		t.Fatalf("valid pending publication audit state: %v", err)
	}

	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER publications_guard_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `
UPDATE publications SET result_sha = 'cccccccccccccccccccccccccccccccccccccccc'
WHERE id = ?`, publication.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyAuditChain(ctx); err != nil {
		t.Fatalf("publication-only tamper unexpectedly changed audit chain: %v", err)
	}
	if _, err := s.VerifyAuditState(ctx); err == nil || !strings.Contains(err.Error(), "differs from its chained audit intent") {
		t.Fatalf("tampered pending publication audit state error = %v", err)
	}
	if _, _, err := s.VerifyAuditMutationState(ctx, nil, nil, acceptAuditMutationState); err == nil || !strings.Contains(err.Error(), "differs from its chained audit intent") {
		t.Fatalf("tampered pending publication mutation state error = %v", err)
	}
}

func TestNewBranchRunWaitsForPendingPublicationOwner(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	const (
		branch       = "feature/publication-owner"
		otherBranch  = "feature/independent"
		previous     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		firstSHA     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		nextSHA      = "cccccccccccccccccccccccccccccccccccccccc"
		unrelatedSHA = "dddddddddddddddddddddddddddddddddddddddd"
	)
	first, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, branch, firstSHA))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRunJobName(ctx, first.ID, "completed-job"); err != nil {
		t.Fatal(err)
	}
	publication, err := s.BeginPublication(ctx, model.PublicationSpec{
		RepoID: repo.ID, RunID: first.ID, RefKind: model.RefBranch, Ref: branch,
		PreviousSHA: previous, ResultSHA: firstSHA, Actor: first.Actor,
	})
	if err != nil {
		t.Fatal(err)
	}

	replacement, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, branch, nextSHA))
	if err != nil {
		t.Fatal(err)
	}
	if len(replacement.Cancellations) != 0 {
		t.Fatalf("publication owner cancellations = %#v, want none", replacement.Cancellations)
	}
	unrelated, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, otherBranch, unrelatedSHA))
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.Run(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if owner.Status != model.RunRunning || owner.Phase != "publishing" || owner.SupersededBy != "" {
		t.Fatalf("publication owner after newer enqueue = %#v", owner)
	}
	queued, err := s.Run(ctx, replacement.ID)
	if err != nil {
		t.Fatal(err)
	}
	if queued.Status != model.RunQueued {
		t.Fatalf("newer run status = %q, want queued", queued.Status)
	}
	pending, err := s.PendingPublications(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != publication.ID {
		t.Fatalf("pending publication = %#v, %v", pending, err)
	}
	cancellations, err := s.PendingRunCancellations(ctx)
	if err != nil || len(cancellations) != 0 {
		t.Fatalf("pending cancellations = %#v, %v", cancellations, err)
	}
	claimed, err := s.ClaimNextRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != unrelated.ID {
		t.Fatalf("claim while publication pending = %s, want unrelated run %s", claimed.ID, unrelated.ID)
	}
	if _, err := s.FinishRun(ctx, unrelated.ID, model.RunResult{Status: model.RunFailed, Phase: "test"}); err != nil {
		t.Fatal(err)
	}
	stillQueued, err := s.Run(ctx, replacement.ID)
	if err != nil || stillQueued.Status != model.RunQueued {
		t.Fatalf("same-ref run while publication pending = %#v, %v", stillQueued, err)
	}

	finalized, err := s.FinalizePublication(ctx, publication.ID, model.PublicationDelivered, "")
	if err != nil {
		t.Fatal(err)
	}
	if finalized.Run.Status != model.RunPassed {
		t.Fatalf("publication owner final status = %q, want passed", finalized.Run.Status)
	}
	next, err := s.ClaimNextRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != replacement.ID || next.Status != model.RunRunning {
		t.Fatalf("next claimed run = %#v, want replacement %s", next, replacement.ID)
	}
}

func TestPromotionPublicationFailureFinalizesLinkedRunAndPromotion(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	const (
		previous = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		result   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	enqueued, promotion, err := s.EnqueuePromotionRun(ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "promotion/main/bbbbbbbbbbbb",
		SHA: result, Actor: "agent@host", Trigger: "promotion", TestedSHA: result, BaseSHA: previous,
	}, model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/durable", SourceSHA: result,
		TargetRef: "main", PreviousSHA: previous, ResultSHA: result, Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	publication, err := s.BeginPublication(ctx, model.PublicationSpec{
		RepoID: repo.ID, RunID: enqueued.ID, PromotionID: promotion.ID,
		RefKind: model.RefBranch, Ref: promotion.TargetRef,
		PreviousSHA: previous, ResultSHA: result, Actor: promotion.Actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	finalized, err := s.FinalizePublication(ctx, publication.ID, model.PublicationFailed, "upstream moved")
	if err != nil {
		t.Fatal(err)
	}
	if finalized.Run.Status != model.RunFailed || finalized.Run.Phase != "publishing" ||
		finalized.Promotion.Status != model.PromotionFailed ||
		!strings.Contains(finalized.Run.Error, "upstream moved") ||
		!strings.Contains(finalized.Promotion.Error, "upstream moved") {
		t.Fatalf("failed publication finalization = %#v", finalized)
	}
}

func TestRestartTerminalizesPromotionWhoseRunningCIHasNoPublicationIntent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "oberth.db")
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	const (
		previous = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		result   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	enqueued, promotion, err := s.EnqueuePromotionRun(ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "promotion/main/bbbbbbbbbbbb",
		SHA: result, Actor: "agent@host", Trigger: "promotion", TestedSHA: result, BaseSHA: previous,
	}, model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/restart", SourceSHA: result,
		TargetRef: "main", PreviousSHA: previous, ResultSHA: result, Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	now = now.Add(time.Minute)
	s, err = Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	recoveredRun, err := s.Run(ctx, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	recoveredPromotion, err := s.Promotion(ctx, promotion.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredRun.Status != model.RunInterrupted || recoveredPromotion.Status != model.PromotionInterrupted ||
		recoveredPromotion.RunID != enqueued.ID {
		t.Fatalf("restart recovery = run %#v, promotion %#v", recoveredRun, recoveredPromotion)
	}
}

func TestPromotionRunFailureIsAlreadyAtomicBeforeRestart(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "oberth.db")
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	const (
		previous = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		result   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	enqueued, promotion, err := s.EnqueuePromotionRun(ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "promotion/main/bbbbbbbbbbbb",
		SHA: result, Actor: "agent@host", Trigger: "promotion", TestedSHA: result, BaseSHA: previous,
	}, model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/terminal", SourceSHA: result,
		TargetRef: "main", PreviousSHA: previous, ResultSHA: result, Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishRun(ctx, enqueued.ID, model.RunResult{Status: model.RunFailed, Error: "tests failed"}); err != nil {
		t.Fatal(err)
	}
	beforeRestart, err := s.Promotion(ctx, promotion.ID)
	if err != nil || beforeRestart.Status != model.PromotionFailed || beforeRestart.Error != "tests failed" {
		t.Fatalf("atomic promotion failure = %#v, %v", beforeRestart, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	now = now.Add(time.Minute)
	s, err = Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	recovered, err := s.Promotion(ctx, promotion.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != model.PromotionFailed || recovered.Error != "tests failed" {
		t.Fatalf("terminal run promotion recovery = %#v", recovered)
	}
}

func TestFastForwardPromotionCanOwnPublicationWithoutRun(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	promotion, err := s.AppendPromotion(ctx, model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/fast", SourceSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		TargetRef: "main", PreviousSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ResultSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	publication, err := s.BeginPublication(ctx, model.PublicationSpec{
		RepoID: repo.ID, PromotionID: promotion.ID, RefKind: model.RefBranch, Ref: "main",
		PreviousSHA: promotion.PreviousSHA, ResultSHA: promotion.ResultSHA, Actor: promotion.Actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	finalized, err := s.FinalizePublication(ctx, publication.ID, model.PublicationDelivered, "")
	if err != nil {
		t.Fatal(err)
	}
	if finalized.Run.ID != "" || finalized.Promotion.Status != model.PromotionPassed {
		t.Fatalf("fast-forward finalization = %#v", finalized)
	}
}

func TestPublishFailureLeavesDeliveredBurnsIntact(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	run, err := s.EnqueueRun(ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature/burn-intact",
		SHA: sha, Actor: "agent@host", Trigger: "branch", TestedSHA: sha,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	// Record step results (simulating burns that passed).
	for _, step := range []model.StepResult{
		{RunID: run.ID, Burn: "test", Step: "unit", Status: model.StepPassed, DeclaredSize: "M"},
		{RunID: run.ID, Burn: "build", Step: "compile", Status: model.StepPassed, DeclaredSize: "M"},
	} {
		if _, err := s.PutStepResult(ctx, step); err != nil {
			t.Fatal(err)
		}
	}
	publication, err := s.BeginPublication(ctx, model.PublicationSpec{
		RepoID: repo.ID, RunID: run.ID, RefKind: model.RefBranch, Ref: run.Ref,
		PreviousSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ResultSHA: sha,
		Actor: run.Actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	finalized, err := s.FinalizePublication(ctx, publication.ID, model.PublicationFailed, "Forgejo: Internal Server Connection Error")
	if err != nil {
		t.Fatal(err)
	}
	if finalized.Run.Status != model.RunFailed || finalized.Run.Phase != "publishing" {
		t.Fatalf("failed publication run = %s/%s, want failed/publishing", finalized.Run.Status, finalized.Run.Phase)
	}
	// Step results must survive the publication failure.
	steps, err := s.StepResults(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 {
		t.Fatalf("step results after publish failure = %d, want 2", len(steps))
	}
	for _, step := range steps {
		if step.Status != model.StepPassed {
			t.Fatalf("step %s/%s = %s after publish failure, want passed", step.Burn, step.Step, step.Status)
		}
	}
}

func TestRetryFailedPublicationSucceeds(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	run, err := s.EnqueueRun(ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature/retry",
		SHA: sha, Actor: "agent@host", Trigger: "branch", TestedSHA: sha,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	publication, err := s.BeginPublication(ctx, model.PublicationSpec{
		RepoID: repo.ID, RunID: run.ID, RefKind: model.RefBranch, Ref: run.Ref,
		PreviousSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ResultSHA: sha,
		Actor: run.Actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err := s.FinalizePublication(ctx, publication.ID, model.PublicationFailed, "forge 500"); err != nil {
		t.Fatal(err)
	}
	// The run should be failed/publishing now.
	failed, err := s.Run(ctx, run.ID)
	if err != nil || failed.Status != model.RunFailed || failed.Phase != "publishing" {
		t.Fatalf("run after publish failure = %s/%s, want failed/publishing", failed.Status, failed.Phase)
	}

	// Retry the publication.
	now = now.Add(time.Second)
	retryPub, err := s.RetryFailedPublication(ctx, run.ID, "", "retry-agent@host")
	if err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if retryPub.Status != model.PublicationPending || retryPub.RunID != run.ID {
		t.Fatalf("retry publication = %#v", retryPub)
	}
	// The run should be back to running/publishing.
	reactivated, err := s.Run(ctx, run.ID)
	if err != nil || reactivated.Status != model.RunRunning || reactivated.Phase != "publishing" {
		t.Fatalf("run after retry = %s/%s, want running/publishing", reactivated.Status, reactivated.Phase)
	}

	// The audit gate MUST pass after retry. This is the core invariant that
	// was violated in v0.16.18: publication.retry as the latest action broke
	// the gate permanently.
	if _, err := s.VerifyAuditState(ctx); err != nil {
		t.Fatalf("audit state after retry: %v", err)
	}
	if _, _, err := s.VerifyAuditMutationStateUnanchored(ctx, nil, nil, acceptAuditMutationState); err != nil {
		t.Fatalf("audit mutation state after retry: %v", err)
	}
}

// TestRetryThenSubsequentMutationSucceeds verifies that a retried publication
// does not poison subsequent unrelated mutations (e.g. issue_create). In v0.16.18
// the inconsistent audit state made every mutation fail.
func TestRetryThenSubsequentMutationSucceeds(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	run, err := s.EnqueueRun(ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature/retry-then-mutate",
		SHA: sha, Actor: "agent@host", Trigger: "branch", TestedSHA: sha,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	publication, err := s.BeginPublication(ctx, model.PublicationSpec{
		RepoID: repo.ID, RunID: run.ID, RefKind: model.RefBranch, Ref: run.Ref,
		PreviousSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ResultSHA: sha,
		Actor: run.Actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err := s.FinalizePublication(ctx, publication.ID, model.PublicationFailed, "forge 500"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err := s.RetryFailedPublication(ctx, run.ID, "", "retry-agent@host"); err != nil {
		t.Fatalf("retry failed: %v", err)
	}

	// A subsequent unrelated mutation (creating an issue) must succeed.
	now = now.Add(time.Second)
	issue, err := s.CreateManualIssue(ctx, "test-actor", model.ManualIssueSpec{
		Title: "test title", Body: "test body",
	})
	if err != nil {
		t.Fatalf("issue_create after retry: %v", err)
	}
	if issue.ID <= 0 {
		t.Fatalf("issue after retry = %#v, want valid ID", issue)
	}

	// Audit chain must be gap-free and verifiable.
	if _, err := s.VerifyAuditChain(ctx); err != nil {
		t.Fatalf("audit chain after retry + mutation: %v", err)
	}
}

// TestStartupReconcileHealsInconsistentRetryState simulates the pre-fix
// inconsistent state (pending row + latest action publication.retry) in a
// fixture DB and verifies that store open heals it: the gate passes, the run
// row is consistent, and the audit chain verifies gap-free.
func TestStartupReconcileHealsInconsistentRetryState(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "oberth.db")
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	run, err := s.EnqueueRun(ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature/reconcile",
		SHA: sha, Actor: "agent@host", Trigger: "branch", TestedSHA: sha,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	publication, err := s.BeginPublication(ctx, model.PublicationSpec{
		RepoID: repo.ID, RunID: run.ID, RefKind: model.RefBranch, Ref: run.Ref,
		PreviousSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ResultSHA: sha,
		Actor: run.Actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err := s.FinalizePublication(ctx, publication.ID, model.PublicationFailed, "forge 500"); err != nil {
		t.Fatal(err)
	}

	// Simulate the v0.16.18 defective retry: reset publication to pending,
	// reactivate the run, and chain ONLY publication.retry as the latest
	// action (without a subsequent publication.pending). This is exactly what
	// the buggy RetryFailedPublication did.
	now = now.Add(time.Second)
	nowNano := now.UnixNano()
	tx, txErr := s.db.BeginTx(ctx, nil)
	if txErr != nil {
		t.Fatal(txErr)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE runs SET status = 'running', phase = 'publishing', error = '', finished_at = NULL, updated_at = ?
WHERE id = ?`, nowNano, run.ID); err != nil {
		t.Fatal(err)
	}
	buggyPub, scanErr := scanPublication(tx.QueryRowContext(ctx, `
UPDATE publications SET status = 'pending', error = '', previous_sha = '', previous_known = 0,
    actor = 'retry-agent@host', updated_at = ?
WHERE id = ? AND status = 'failed'
RETURNING `+publicationColumns, nowNano, publication.ID))
	if scanErr != nil {
		t.Fatal(scanErr)
	}
	// Chain only publication.retry — the buggy action.
	if err := (&Store{protocol: DefaultProtocolConfig()}).appendPublicationAudit(ctx, tx, buggyPub, "publication.retry", "retrying failed publication "+publication.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Confirm the gate is now broken (as it was in v0.16.18).
	if _, err := s.VerifyAuditState(ctx); err == nil {
		t.Fatal("expected audit state to be inconsistent after simulated v0.16.18 retry")
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Re-open the store. The startup reconciliation must heal the state.
	now = now.Add(time.Minute)
	s, err = Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("open after simulated v0.16.18 state: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// The gate must now pass.
	if _, err := s.VerifyAuditState(ctx); err != nil {
		t.Fatalf("audit state after reconciliation: %v", err)
	}
	if _, _, err := s.VerifyAuditMutationStateUnanchored(ctx, nil, nil, acceptAuditMutationState); err != nil {
		t.Fatalf("audit mutation state after reconciliation: %v", err)
	}

	// The audit chain must be gap-free.
	if _, err := s.VerifyAuditChain(ctx); err != nil {
		t.Fatalf("audit chain after reconciliation: %v", err)
	}

	// The run must still be running/publishing (the reconciliation healed
	// the publication, not the run).
	reactivated, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reactivated.Status != model.RunRunning || reactivated.Phase != "publishing" {
		t.Fatalf("run after reconciliation = %s/%s, want running/publishing", reactivated.Status, reactivated.Phase)
	}

	// The pending publication must exist.
	pending, err := s.PendingPublications(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != publication.ID {
		t.Fatalf("pending publications after reconciliation = %#v, %v", pending, err)
	}
}

// TestStartupReconcileIsIdempotent verifies that reconciliation is a no-op on
// an already-consistent store (no extra audit actions appended).
func TestStartupReconcileIsIdempotent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "oberth.db")
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	run, err := s.EnqueueRun(ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature/idempotent",
		SHA: sha, Actor: "agent@host", Trigger: "branch", TestedSHA: sha,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginPublication(ctx, model.PublicationSpec{
		RepoID: repo.ID, RunID: run.ID, RefKind: model.RefBranch, Ref: run.Ref,
		PreviousSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ResultSHA: sha,
		Actor: run.Actor,
	}); err != nil {
		t.Fatal(err)
	}

	// Record the audit head before reconciliation.
	headBefore, err := s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Run reconciliation on an already-consistent store.
	healed, err := s.ReconcileInconsistentPublications(ctx)
	if err != nil {
		t.Fatalf("reconcile consistent store: %v", err)
	}
	if healed != 0 {
		t.Fatalf("reconcile healed %d, want 0 (already consistent)", healed)
	}

	// Audit head must not have changed.
	headAfter, err := s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if headBefore.ID != headAfter.ID {
		t.Fatalf("audit head changed from %d to %d on idempotent reconciliation", headBefore.ID, headAfter.ID)
	}
}

func TestRetryRefusedForNonPublishingFailedRun(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	run, err := s.EnqueueRun(ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature/no-retry",
		SHA: sha, Actor: "agent@host", Trigger: "branch", TestedSHA: sha,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	// Finish the run as failed in the "test" phase (code failure, not publish).
	now = now.Add(time.Second)
	if _, err := s.FinishRun(ctx, run.ID, model.RunResult{
		Status: model.RunFailed, Phase: "test", Error: "unit tests failed",
	}); err != nil {
		t.Fatal(err)
	}
	// There's no failed publication for this run.
	_, err = s.RetryFailedPublication(ctx, run.ID, "", "retry-agent@host")
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("retry for code-failed run error = %v, want ErrInvalidState", err)
	}
}

func TestRestartTerminalizesFastForwardPromotionWithoutPublicationIntent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "oberth.db")
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	promotion, err := s.AppendPromotion(ctx, model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/orphan", SourceSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		TargetRef: "main", PreviousSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ResultSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	now = now.Add(time.Minute)
	s, err = Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	recovered, err := s.Promotion(ctx, promotion.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != model.PromotionInterrupted || !strings.Contains(recovered.Error, "before promotion publication intent") {
		t.Fatalf("orphan fast-forward promotion = %#v", recovered)
	}
}
