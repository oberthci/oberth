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
