package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func cancelTestRun(t *testing.T, s *Store, repo model.Repository, ref, digit string) model.Run {
	t.Helper()
	enqueued, err := s.EnqueueRun(context.Background(), model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch,
		Ref: ref, SHA: strings.Repeat(digit, 40), Actor: "pusher@host", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	return enqueued.Run
}

func assertNoOpenCIIssue(t *testing.T, s *Store, repo model.Repository, ref string) {
	t.Helper()
	if issue, err := s.OpenCIIssue(context.Background(), repo.ID, ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel opened CI issue %#v (%v)", issue, err)
	}
}

func TestCancelQueuedRunTerminalizesWithActorReasonAndAuditWithoutObligationOrIssue(t *testing.T) {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()
	repo := createRepo(t, s)
	run := cancelTestRun(t, s, repo, "feature/cancel", "a")

	result, err := s.CancelRun(ctx, model.RunCancelRequest{RunID: run.ID, Actor: "dima@host", Reason: "  wrong branch  "})
	if err != nil {
		t.Fatal(err)
	}
	cancelled := result.Run
	if cancelled.Status != model.RunInterrupted || cancelled.Phase != model.PhaseCancelled || !cancelled.Cancelled() ||
		!cancelled.Preempted() || cancelled.Reason != "cancelled by dima@host: wrong branch" ||
		cancelled.Error != cancelled.Reason || cancelled.FinishedAt == nil || cancelled.SupersededBy != "" {
		t.Fatalf("cancelled queued run = %#v", cancelled)
	}
	if result.PreviousState != model.RunQueued || len(result.Cancellations) != 0 {
		t.Fatalf("queued cancel result = %#v", result)
	}
	if pending, err := s.PendingRunCancellations(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("queued cancel recorded an obligation: %#v, %v", pending, err)
	}
	audit, err := s.LatestAuditActionByType(ctx, "run.cancel", "run")
	if err != nil || audit.Actor != "dima@host" || audit.ResourceID != run.ID {
		t.Fatalf("cancel audit = %#v, %v", audit, err)
	}
	var details map[string]any
	if err := json.Unmarshal([]byte(audit.Details), &details); err != nil {
		t.Fatal(err)
	}
	if details["reason"] != "wrong branch" || details["previous_state"] != "queued" || details["ref"] != "feature/cancel" {
		t.Fatalf("cancel audit details = %v", details)
	}
	assertNoOpenCIIssue(t, s, repo, "feature/cancel")
	if claimed, err := s.ClaimNextRun(ctx); err == nil {
		t.Fatalf("a cancelled queued run was claimed: %#v", claimed)
	}
}

func TestCancelTerminalRunIsAClearNoOpError(t *testing.T) {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()
	repo := createRepo(t, s)
	cancelled := cancelTestRun(t, s, repo, "feature/twice", "a")
	if _, err := s.CancelRun(ctx, model.RunCancelRequest{RunID: cancelled.ID, Actor: "dima@host"}); err != nil {
		t.Fatal(err)
	}
	before, err := s.LatestAuditActionByType(ctx, "run.cancel", "run")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CancelRun(ctx, model.RunCancelRequest{RunID: cancelled.ID, Actor: "other@host", Reason: "again"})
	if !errors.Is(err, ErrRunNotActive) || !errors.Is(err, ErrInvalidState) || !strings.Contains(err.Error(), "already cancelled") {
		t.Fatalf("second cancel = %v", err)
	}
	after, err := s.LatestAuditActionByType(ctx, "run.cancel", "run")
	if err != nil || after.ID != before.ID {
		t.Fatalf("a refused cancel appended audit %#v (%v)", after, err)
	}
	if again, err := s.Run(ctx, cancelled.ID); err != nil || again.Reason != "cancelled by dima@host" {
		t.Fatalf("refused cancel changed the run: %#v, %v", again, err)
	}

	passed := cancelTestRun(t, s, repo, "feature/green", "b")
	if _, err := s.FinishRun(ctx, passed.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelRun(ctx, model.RunCancelRequest{RunID: passed.ID, Actor: "dima@host"}); !errors.Is(err, ErrRunNotActive) ||
		!strings.Contains(err.Error(), "already passed") {
		t.Fatalf("cancel of a passed run = %v", err)
	}
	if _, err := s.CancelRun(ctx, model.RunCancelRequest{RunID: strings.Repeat("f", 32), Actor: "dima@host"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel of a missing run = %v", err)
	}
	for _, reason := range []string{strings.Repeat("x", model.MaximumCancelReasonBytes+1), "two\nlines", "nul\x00"} {
		if _, err := s.CancelRun(ctx, model.RunCancelRequest{RunID: passed.ID, Actor: "dima@host", Reason: reason}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("reason %q accepted: %v", reason, err)
		}
	}
}

// A claimed run receives the supersede obligation with no successor, keeps the
// Job name its owner published, and is never re-fired by owner restart
// recovery: restart requeue selects only active runs.
func TestCancelClaimedRunRecordsDeletionObligationAndIsNotRefiredAtRestart(t *testing.T) {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()
	repo := createRepo(t, s)
	withJob := cancelTestRun(t, s, repo, "feature/running", "a")
	withoutJob := cancelTestRun(t, s, repo, "feature/waiting", "b")
	for range 2 {
		if _, err := s.ClaimNextRun(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SetRunJobName(ctx, withJob.ID, "oberth-job-running"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkRunSubmitted(ctx, withJob.ID); err != nil {
		t.Fatal(err)
	}

	running, err := s.CancelRun(ctx, model.RunCancelRequest{RunID: withJob.ID, Actor: "dima@host", Reason: "stuck"})
	if err != nil {
		t.Fatal(err)
	}
	if running.PreviousState != model.RunRunning || len(running.Cancellations) != 1 ||
		running.Cancellations[0].JobName != "oberth-job-running" || running.Cancellations[0].SupersededBy != "" {
		t.Fatalf("running cancel = %#v", running)
	}
	waiting, err := s.CancelRun(ctx, model.RunCancelRequest{RunID: withoutJob.ID, Actor: "dima@host"})
	if err != nil {
		t.Fatal(err)
	}
	if waiting.PreviousState != model.RunWaiting || len(waiting.Cancellations) != 1 || waiting.Cancellations[0].JobName != "" {
		t.Fatalf("waiting cancel = %#v", waiting)
	}
	// The owner of the waiting run may still publish its Job name; the
	// obligation follows it, and MarkRunSubmitted hands back the cancelled
	// record instead of starting it.
	named, err := s.SetRunJobName(ctx, withoutJob.ID, "oberth-job-waiting")
	if err != nil || !named.Cancelled() {
		t.Fatalf("job name after cancel = %#v, %v", named, err)
	}
	if submitted, err := s.MarkRunSubmitted(ctx, withoutJob.ID); err != nil || !submitted.Cancelled() || submitted.StartedAt != nil {
		t.Fatalf("submission after cancel = %#v, %v", submitted, err)
	}
	pending, err := s.PendingRunCancellations(ctx)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending cancellations = %#v, %v", pending, err)
	}
	for _, obligation := range pending {
		if obligation.JobName == "" {
			t.Fatalf("obligation lost its Job name: %#v", obligation)
		}
	}

	if err := s.RecoverOwnerState(ctx); err != nil {
		t.Fatal(err)
	}
	runs, err := s.ListRecentRuns(ctx, model.RunListFilter{RepoID: repo.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("restart re-fired a cancelled run: %d runs", len(runs))
	}
	for _, run := range runs {
		if !run.Cancelled() {
			t.Fatalf("restart rewrote a cancelled run: %#v", run)
		}
	}
	for _, obligation := range pending {
		if err := s.CompleteRunCancellation(ctx, obligation.RunID, obligation.JobName); err != nil {
			t.Fatal(err)
		}
	}
	assertNoOpenCIIssue(t, s, repo, "feature/running")
	assertNoOpenCIIssue(t, s, repo, "feature/waiting")
	// A newer push of the same branch is not superseding anything any more.
	enqueued, err := s.EnqueueRun(ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch,
		Ref: "feature/running", SHA: strings.Repeat("c", 40), Actor: "pusher@host", Trigger: "branch"})
	if err != nil || len(enqueued.Cancellations) != 0 {
		t.Fatalf("newer push after cancel = %#v, %v", enqueued.Cancellations, err)
	}
	if again, _ := s.Run(ctx, withJob.ID); again.SupersededBy != "" || !again.Cancelled() {
		t.Fatalf("supersede rewrote a cancelled run: %#v", again)
	}
}

// A cancel that wins before the owner publishes a Job name is settled at
// restart without a Kubernetes deletion, like a superseded one.
func TestRestartSettlesNoJobCancellationOfACancelledRun(t *testing.T) {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()
	repo := createRepo(t, s)
	run := cancelTestRun(t, s, repo, "feature/no-job", "a")
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelRun(ctx, model.RunCancelRequest{RunID: run.ID, Actor: "dima@host"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteSupersededRunCancellationWithoutJob(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.PendingRunCancellations(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("owner exit left the no-Job obligation pending: %#v, %v", pending, err)
	}

	second := cancelTestRun(t, s, repo, "feature/no-job-restart", "b")
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelRun(ctx, model.RunCancelRequest{RunID: second.ID, Actor: "dima@host"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverOwnerState(ctx); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.PendingRunCancellations(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("restart left the no-Job obligation pending: %#v, %v", pending, err)
	}
}

// Cancelling promotion CI fails the promotion in the same transaction, so the
// owner can never begin a publication for it, and nothing is merged.
func TestCancelPromotionRunFailsThePromotionAndBlocksPublication(t *testing.T) {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()
	repo := createRepo(t, s)
	const (
		previous = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		result   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	enqueued, promotion, err := s.EnqueuePromotionRun(ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "promotion/main/bbbbbbbbbbbb",
		SHA: result, Actor: "agent@host", Trigger: "promotion", TestedSHA: result, BaseSHA: previous,
	}, model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/promote", SourceSHA: result,
		TargetRef: "main", PreviousSHA: previous, ResultSHA: result, Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	cancelled, err := s.CancelRun(ctx, model.RunCancelRequest{RunID: enqueued.ID, Actor: "admin@host", Reason: "bad merge"})
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.PromotionID != promotion.ID {
		t.Fatalf("promotion cancel result = %#v", cancelled)
	}
	failed, err := s.Promotion(ctx, promotion.ID)
	if err != nil || failed.Status != model.PromotionFailed || !strings.Contains(failed.Error, "cancelled by admin@host: bad merge") {
		t.Fatalf("promotion after cancel = %#v, %v", failed, err)
	}
	if _, err := s.BeginPublication(ctx, model.PublicationSpec{
		RepoID: repo.ID, RunID: enqueued.ID, PromotionID: promotion.ID, RefKind: model.RefBranch, Ref: "main",
		PreviousSHA: previous, ResultSHA: result, Actor: "agent@host",
	}); err == nil {
		t.Fatal("a cancelled promotion run began its publication")
	}
	if issue, err := s.OpenCIIssue(ctx, repo.ID, "feature/promote"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("promotion cancel opened issue %#v (%v)", issue, err)
	}
}

// A passed run that owns a pending publication belongs to the publication: a
// cancel cannot unpublish a push already in flight.
func TestCancelRefusesARunOwnedByAPendingPublication(t *testing.T) {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()
	repo := createRepo(t, s)
	run := cancelTestRun(t, s, repo, "feature/publishing", "a")
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginPublication(ctx, model.PublicationSpec{RepoID: repo.ID, RunID: run.ID, RefKind: model.RefBranch,
		Ref: run.Ref, ResultSHA: run.SHA, Actor: run.Actor}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelRun(ctx, model.RunCancelRequest{RunID: run.ID, Actor: "dima@host"}); !errors.Is(err, ErrInvalidState) ||
		!strings.Contains(err.Error(), "pending upstream publication") {
		t.Fatalf("cancel of a publishing run = %v", err)
	}
	if current, err := s.Run(ctx, run.ID); err != nil || current.Status != model.RunRunning {
		t.Fatalf("refused cancel changed the run: %#v, %v", current, err)
	}
}

// The authorization callback sees the run as it is inside the cancel
// transaction: a concurrency group recorded after the caller's read refuses a
// non-admin cancel without any change.
func TestCancelRunAuthorizesTheCurrentRecord(t *testing.T) {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()
	repo := createRepo(t, s)
	run := cancelTestRun(t, s, repo, "feature/grouped", "a")
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunConcurrencyGroup(ctx, run.ID, "terraform-state"); err != nil {
		t.Fatal(err)
	}
	refused := errors.New("refused")
	var seen model.Run
	_, err := s.CancelRun(ctx, model.RunCancelRequest{RunID: run.ID, Actor: "agent@host",
		Authorize: func(current model.Run) (string, error) { seen = current; return "", refused }})
	if !errors.Is(err, refused) || seen.ConcurrencyGroup != "terraform-state" {
		t.Fatalf("authorize saw %#v, err %v", seen, err)
	}
	if current, err := s.Run(ctx, run.ID); err != nil || current.Status != model.RunRunning {
		t.Fatalf("refused cancel changed the run: %#v, %v", current, err)
	}
	result, err := s.CancelRun(ctx, model.RunCancelRequest{RunID: run.ID, Actor: "dima@host",
		Authorize: func(model.Run) (string, error) { return "group warning", nil }})
	if err != nil {
		t.Fatal(err)
	}
	audit, err := s.LatestAuditActionByType(ctx, "run.cancel", "run")
	if err != nil || !strings.Contains(audit.Details, "group warning") || result.Run.ID != run.ID {
		t.Fatalf("authorized cancel audit = %#v, %v", audit, err)
	}
}

// An owner whose deletion failed after a pass settled the obligation re-arms
// it, so the next pass deletes the Workflow; an active run is never armed.
func TestRearmRunCancellationReopensOnlyPreemptedRuns(t *testing.T) {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()
	repo := createRepo(t, s)
	run := cancelTestRun(t, s, repo, "feature/rearm", "a")
	active := cancelTestRun(t, s, repo, "feature/active", "b")
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelRun(ctx, model.RunCancelRequest{RunID: run.ID, Actor: "dima@host"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteSupersededRunCancellationWithoutJob(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RearmRunCancellation(ctx, run.ID, "oberth-late-workflow"); err != nil {
		t.Fatal(err)
	}
	if err := s.RearmRunCancellation(ctx, active.ID, "oberth-active"); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingRunCancellations(ctx)
	if err != nil || len(pending) != 1 || pending[0].RunID != run.ID || pending[0].JobName != "oberth-late-workflow" {
		t.Fatalf("re-armed obligations = %#v, %v", pending, err)
	}
}
