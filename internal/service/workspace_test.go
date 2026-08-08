package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func TestSchedulerRemovesRunWorkspaceOnlyAfterTerminalOutcome(t *testing.T) {
	fixture := newControlFixture(t, JobResult{Status: model.RunFailed, Phase: "test", Error: "red"})
	ctx := context.Background()
	enqueued, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "workspace-terminal", Repository: fixture.repo, Branch: "feature/workspace-terminal",
		SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(fixture.root, "work", enqueued.ID)
	if err := fixture.scheduler.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	finished, err := fixture.store.Run(ctx, enqueued.ID)
	if err != nil || finished.Status != model.RunFailed {
		t.Fatalf("terminal run = %#v, %v", finished, err)
	}
	assertWorkspaceMissing(t, workspace)
}

func TestWorkspaceCleanupFailureDoesNotRepaintTerminalRun(t *testing.T) {
	fixture := newControlFixture(t, JobResult{Status: model.RunFailed, Phase: "test", Error: "red"})
	ctx := context.Background()
	cleanupErr := errors.New("injected workspace cleanup failure")
	var failedPath string
	scheduler, err := NewScheduler(SchedulerConfig{
		Store: fixture.store, Git: fixture.git, Logs: fixture.logs, Jobs: fixture.jobs, ReleaseJobs: fixture.jobs,
		Auditor: fixture.store, Signals: fixture.signals, WorkspaceRoot: filepath.Join(fixture.root, "work"),
		MaxConcurrent: 1,
		RemoveWorkspace: func(path string) error {
			if path == failedPath {
				return cleanupErr
			}
			return os.RemoveAll(path)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	enqueued, err := scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "workspace-cleanup-failure", Repository: fixture.repo, Branch: "feature/workspace-cleanup-failure",
		SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	failedPath = filepath.Join(fixture.root, "work", enqueued.ID)
	if err := scheduler.ProcessNext(ctx); !errors.Is(err, cleanupErr) {
		t.Fatalf("cleanup error = %v, want %v", err, cleanupErr)
	}
	finished, err := fixture.store.Run(ctx, enqueued.ID)
	if err != nil || finished.Status != model.RunFailed {
		t.Fatalf("run after cleanup failure = %#v, %v", finished, err)
	}
	assertWorkspaceExists(t, failedPath)

	restarted, err := NewScheduler(SchedulerConfig{
		Store: fixture.store, Git: fixture.git, Logs: fixture.logs, Jobs: fixture.jobs, ReleaseJobs: fixture.jobs,
		Auditor: fixture.store, Signals: fixture.signals, WorkspaceRoot: filepath.Join(fixture.root, "work"),
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.recoverOwnedWorkspaces(ctx); err != nil {
		t.Fatal(err)
	}
	assertWorkspaceMissing(t, failedPath)
}

func TestWorkspaceStartupRecoveryDeletesOnlyDurablyTerminalOwners(t *testing.T) {
	fixture := newControlFixture(t)
	ctx := context.Background()
	root := filepath.Join(fixture.root, "work")

	terminal := enqueueWorkspaceRun(t, fixture, "feature/terminal", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if _, err := fixture.store.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.FinishRun(ctx, terminal.ID, model.RunResult{Status: model.RunFailed, Error: "red"}); err != nil {
		t.Fatal(err)
	}

	running := enqueueWorkspaceRun(t, fixture, "feature/running", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if _, err := fixture.store.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	publishing := enqueueWorkspaceRun(t, fixture, "feature/publishing", "cccccccccccccccccccccccccccccccccccccccc")
	if _, err := fixture.store.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	publication, err := fixture.store.BeginPublication(ctx, model.PublicationSpec{
		RepoID: fixture.repo.ID, RunID: publishing.ID, RefKind: model.RefBranch,
		Ref: publishing.Ref, ResultSHA: publishing.SHA, Actor: publishing.Actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if publication.Status != model.PublicationPending {
		t.Fatalf("publication = %#v", publication)
	}
	queued := enqueueWorkspaceRun(t, fixture, "feature/queued", "dddddddddddddddddddddddddddddddddddddddd")

	pendingPromotion, err := fixture.store.AppendPromotion(ctx, model.PromotionSpec{
		RepoID: fixture.repo.ID, SourceBranch: "feature/pending-promotion",
		SourceSHA: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", TargetRef: "main", Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	terminalPromotion, err := fixture.store.AppendPromotion(ctx, model.PromotionSpec{
		RepoID: fixture.repo.ID, SourceBranch: "feature/terminal-promotion",
		SourceSHA: "ffffffffffffffffffffffffffffffffffffffff", TargetRef: "main", Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.FinishPromotion(ctx, terminalPromotion.ID, model.PromotionFailed, "", "planning failed"); err != nil {
		t.Fatal(err)
	}

	paths := map[string]string{
		"terminal run":       filepath.Join(root, terminal.ID),
		"running run":        filepath.Join(root, running.ID),
		"publishing run":     filepath.Join(root, publishing.ID),
		"queued run":         filepath.Join(root, queued.ID),
		"pending promotion":  mustPromotionWorkspacePath(t, root, pendingPromotion.ID),
		"terminal promotion": mustPromotionWorkspacePath(t, root, terminalPromotion.ID),
		"unknown owner":      filepath.Join(root, "promotion-unknown-owner"),
	}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Join(path, "src"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.scheduler.recoverOwnedWorkspaces(ctx); err != nil {
		t.Fatal(err)
	}
	assertWorkspaceMissing(t, paths["terminal run"])
	assertWorkspaceMissing(t, paths["terminal promotion"])
	for _, owner := range []string{"running run", "publishing run", "queued run", "pending promotion", "unknown owner"} {
		assertWorkspaceExists(t, paths[owner])
	}
}

func TestWorkspaceStartupRecoveryPreservesPendingPublication(t *testing.T) {
	fixture := newControlFixture(t)
	ctx := context.Background()
	root := filepath.Join(fixture.root, "work")
	databasePath := filepath.Join(fixture.root, "oberth.sqlite")

	interrupted := enqueueWorkspaceRun(t, fixture, "feature/interrupted", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if _, err := fixture.store.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	publicationOwner := enqueueWorkspaceRun(t, fixture, "feature/publication-owner", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if _, err := fixture.store.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.BeginPublication(ctx, model.PublicationSpec{
		RepoID: fixture.repo.ID, RunID: publicationOwner.ID, RefKind: model.RefBranch,
		Ref: publicationOwner.Ref, ResultSHA: publicationOwner.SHA, Actor: publicationOwner.Actor,
	}); err != nil {
		t.Fatal(err)
	}
	promotion, err := fixture.store.AppendPromotion(ctx, model.PromotionSpec{
		RepoID: fixture.repo.ID, SourceBranch: "feature/promotion-publication",
		SourceSHA: "cccccccccccccccccccccccccccccccccccccccc", TargetRef: "main", Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	promotion, err = fixture.store.PlanPromotion(ctx, promotion.ID,
		"dddddddddddddddddddddddddddddddddddddddd", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.BeginPublication(ctx, model.PublicationSpec{
		RepoID: fixture.repo.ID, PromotionID: promotion.ID, RefKind: model.RefBranch, Ref: promotion.TargetRef,
		PreviousSHA: promotion.PreviousSHA, ResultSHA: promotion.ResultSHA, Actor: promotion.Actor,
	}); err != nil {
		t.Fatal(err)
	}

	interruptedPath := filepath.Join(root, interrupted.ID)
	publicationPath := filepath.Join(root, publicationOwner.ID)
	promotionPath := mustPromotionWorkspacePath(t, root, promotion.ID)
	for _, path := range []string{interruptedPath, publicationPath, promotionPath} {
		if err := os.MkdirAll(filepath.Join(path, "src"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restarted, err := NewScheduler(SchedulerConfig{
		Store: reopened, Git: fixture.git, Logs: fixture.logs, Jobs: fixture.jobs, ReleaseJobs: fixture.jobs,
		Auditor: reopened, Signals: fixture.signals, WorkspaceRoot: root, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.recoverOwnedWorkspaces(ctx); err != nil {
		t.Fatal(err)
	}
	assertWorkspaceMissing(t, interruptedPath)
	assertWorkspaceExists(t, publicationPath)
	assertWorkspaceExists(t, promotionPath)

	preservedRun, err := reopened.Run(ctx, publicationOwner.ID)
	if err != nil || preservedRun.Status != model.RunRunning || preservedRun.Phase != "publishing" {
		t.Fatalf("pending publication run after restart = %#v, %v", preservedRun, err)
	}
	preservedPromotion, err := reopened.Promotion(ctx, promotion.ID)
	if err != nil || preservedPromotion.Status != model.PromotionPending {
		t.Fatalf("pending publication promotion after restart = %#v, %v", preservedPromotion, err)
	}
}

func TestSupersededWorkspaceWaitsForCancellationCompletion(t *testing.T) {
	fixture := newControlFixture(t)
	ctx := context.Background()
	first := enqueueWorkspaceRun(t, fixture, "feature/superseded-workspace", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if _, err := fixture.store.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	jobName, err := deterministicJobName(fixture.repo.Name, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.SetRunJobName(ctx, first.ID, jobName); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.EnqueueRun(ctx, model.RunSpec{
		RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: first.Ref,
		SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Actor: first.Actor, Trigger: "branch",
	}); err != nil {
		t.Fatal(err)
	}
	interrupted, err := fixture.store.Run(ctx, first.ID)
	if err != nil || interrupted.Status != model.RunInterrupted || interrupted.Phase != "interrupted" || interrupted.SupersededBy == "" {
		t.Fatalf("superseded workspace owner = %#v, %v", interrupted, err)
	}
	workspace := filepath.Join(fixture.root, "work", first.ID)
	if err := os.MkdirAll(filepath.Join(workspace, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fixture.scheduler.recoverOwnedWorkspaces(ctx); err != nil {
		t.Fatal(err)
	}
	assertWorkspaceExists(t, workspace)
	if err := fixture.scheduler.completePendingCancellations(ctx); err != nil {
		t.Fatal(err)
	}
	assertWorkspaceMissing(t, workspace)
}

func TestOwnerRestartCompletesEmptyNameSupersedeBeforeCleanup(t *testing.T) {
	fixture := newControlFixture(t)
	ctx := context.Background()
	root := filepath.Join(fixture.root, "work")
	databasePath := filepath.Join(fixture.root, "oberth.sqlite")
	first := enqueueWorkspaceRun(t, fixture, "feature/pre-job-supersede", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if _, err := fixture.store.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, first.ID)
	if err := os.MkdirAll(filepath.Join(workspace, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.EnqueueRun(ctx, model.RunSpec{
		RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: first.Ref,
		SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Actor: first.Actor, Trigger: "branch",
	}); err != nil {
		t.Fatal(err)
	}
	interrupted, err := fixture.store.Run(ctx, first.ID)
	if err != nil || interrupted.Status != model.RunInterrupted || interrupted.Phase != "interrupted" || interrupted.SupersededBy == "" {
		t.Fatalf("pre-Job superseded workspace owner = %#v, %v", interrupted, err)
	}
	pending, err := fixture.store.PendingRunCancellations(ctx)
	if err != nil || len(pending) != 1 || pending[0].RunID != first.ID || pending[0].JobName != "" {
		t.Fatalf("pre-Job cancellation = %#v, %v", pending, err)
	}
	// The original process may still be checking out until owner shutdown, so
	// ordinary startup recovery cannot complete or clean this empty-name owner.
	if err := fixture.scheduler.completePendingCancellations(ctx); err != nil {
		t.Fatal(err)
	}
	if err := fixture.scheduler.recoverOwnedWorkspaces(ctx); err != nil {
		t.Fatal(err)
	}
	assertWorkspaceExists(t, workspace)

	if err := fixture.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	pending, err = reopened.PendingRunCancellations(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("owner-restart cancellation = %#v, %v", pending, err)
	}
	restarted, err := NewScheduler(SchedulerConfig{
		Store: reopened, Git: fixture.git, Logs: fixture.logs, Jobs: fixture.jobs, ReleaseJobs: fixture.jobs,
		Auditor: reopened, Signals: fixture.signals, WorkspaceRoot: root, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.recoverOwnedWorkspaces(ctx); err != nil {
		t.Fatal(err)
	}
	assertWorkspaceMissing(t, workspace)
}

func TestWorkerJoinCompletesEmptyNameSupersedeBeforeCleanup(t *testing.T) {
	fixture := newControlFixture(t)
	ctx := context.Background()
	root := filepath.Join(fixture.root, "work")
	git := &blockingCheckoutGit{
		controlGit: fixture.git, started: make(chan struct{}), release: make(chan struct{}),
		err: errors.New("checkout failed after supersede"),
	}
	scheduler, err := NewScheduler(SchedulerConfig{
		Store: fixture.store, Git: git, Logs: fixture.logs, Jobs: fixture.jobs, ReleaseJobs: fixture.jobs,
		Auditor: fixture.store, Signals: fixture.signals, WorkspaceRoot: root, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "worker-join-first", Repository: fixture.repo, Branch: "feature/worker-join",
		SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	processed := make(chan error, 1)
	go func() { processed <- scheduler.ProcessNext(ctx) }()
	select {
	case <-git.started:
	case <-time.After(5 * time.Second):
		close(git.release)
		t.Fatal("checkout did not start")
	}
	workspace := filepath.Join(root, first.ID)
	if _, err := scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "worker-join-second", Repository: fixture.repo, Branch: "feature/worker-join",
		OldSHA: first.SHA, SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Actor: "agent@host",
	}); err != nil {
		close(git.release)
		t.Fatal(err)
	}
	pending, err := fixture.store.PendingRunCancellations(ctx)
	if err != nil || len(pending) != 1 || pending[0].RunID != first.ID || pending[0].JobName != "" {
		close(git.release)
		t.Fatalf("live empty-name cancellation = %#v, %v", pending, err)
	}
	if err := scheduler.recoverOwnedWorkspaces(ctx); err != nil {
		close(git.release)
		t.Fatal(err)
	}
	assertWorkspaceExists(t, workspace)
	close(git.release)
	select {
	case err := <-processed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("superseded worker did not return")
	}
	pending, err = fixture.store.PendingRunCancellations(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("worker-join cancellation = %#v, %v", pending, err)
	}
	assertWorkspaceMissing(t, workspace)
}

func TestPromotionOnlyTerminalPathsCleanExactWorkspace(t *testing.T) {
	const (
		sourceSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		baseSHA   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		mergedSHA = "cccccccccccccccccccccccccccccccccccccccc"
	)
	tests := []struct {
		name      string
		configure func(*controlFixture) Auditor
		status    model.PromotionStatus
	}{
		{
			name: "planning failure",
			configure: func(fixture *controlFixture) Auditor {
				fixture.git.prepareErr = errors.New("merge conflict")
				return fixture.store
			},
			status: model.PromotionFailed,
		},
		{
			name: "audit failure",
			configure: func(fixture *controlFixture) Auditor {
				fixture.git.plan = gitcache.MergeCandidate{BaseSHA: baseSHA, MergedSHA: mergedSHA}
				return &promotionAuditFailer{Store: fixture.store, err: errors.New("audit unavailable")}
			},
			status: model.PromotionFailed,
		},
		{
			name: "fast-forward delivered",
			configure: func(fixture *controlFixture) Auditor {
				fixture.git.plan = gitcache.MergeCandidate{BaseSHA: baseSHA, MergedSHA: sourceSHA, FastForward: true}
				return fixture.store
			},
			status: model.PromotionPassed,
		},
		{
			name: "fast-forward failed",
			configure: func(fixture *controlFixture) Auditor {
				fixture.git.plan = gitcache.MergeCandidate{BaseSHA: baseSHA, MergedSHA: sourceSHA, FastForward: true}
				fixture.git.pushErr = errors.New("target moved")
				fixture.git.pushMoveSHA = mergedSHA
				return fixture.store
			},
			status: model.PromotionFailed,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newControlFixture(t)
			seedWorkspacePromotionCandidate(t, fixture, sourceSHA)
			auditor := test.configure(fixture)
			service := newWorkspaceAPI(t, fixture, auditor, nil)
			value, err := service.CallTool(context.Background(), api.Actor{Identity: "agent@host"}, "promote",
				json.RawMessage(`{"sha":"`+sourceSHA+`","branch":"main"}`))
			if err != nil {
				t.Fatal(err)
			}
			promotion := requireToolPromotion(t, fixture, value)
			if promotion.ID == "" || promotion.Status != test.status {
				t.Fatalf("terminal promotion = %#v", promotion)
			}
			assertWorkspaceMissing(t, mustPromotionWorkspacePath(t, filepath.Join(fixture.root, "work"), promotion.ID))
		})
	}
}

func TestSchedulerCleansDivergentPromotionAndRunWorkspaces(t *testing.T) {
	const (
		sourceSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		baseSHA   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		mergedSHA = "cccccccccccccccccccccccccccccccccccccccc"
	)
	fixture := newControlFixture(t, JobResult{Status: model.RunPassed, Phase: "passed"})
	seedWorkspacePromotionCandidate(t, fixture, sourceSHA)
	fixture.git.plan = gitcache.MergeCandidate{BaseSHA: baseSHA, MergedSHA: mergedSHA}
	service := newWorkspaceAPI(t, fixture, fixture.store, nil)
	value, err := service.CallTool(context.Background(), api.Actor{Identity: "agent@host"}, "promote",
		json.RawMessage(`{"sha":"`+sourceSHA+`","branch":"main"}`))
	if err != nil {
		t.Fatal(err)
	}
	promotion := requireToolPromotion(t, fixture, value)
	if promotion.Status != model.PromotionPending || promotion.RunID == "" {
		t.Fatalf("pending promotion = %#v", promotion)
	}
	root := filepath.Join(fixture.root, "work")
	promotionPath := mustPromotionWorkspacePath(t, root, promotion.ID)
	assertWorkspaceExists(t, promotionPath)
	if err := fixture.scheduler.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertWorkspaceMissing(t, promotionPath)
	assertWorkspaceMissing(t, filepath.Join(root, promotion.RunID))
	finished, err := fixture.store.Promotion(context.Background(), promotion.ID)
	if err != nil || finished.Status != model.PromotionPassed {
		t.Fatalf("terminal divergent promotion = %#v, %v", finished, err)
	}
}

func TestPromotionCleanupFailureIsObservableWithoutRepaintingOutcome(t *testing.T) {
	const (
		sourceSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		baseSHA   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	fixture := newControlFixture(t)
	seedWorkspacePromotionCandidate(t, fixture, sourceSHA)
	fixture.git.plan = gitcache.MergeCandidate{BaseSHA: baseSHA, MergedSHA: sourceSHA, FastForward: true}
	cleanupErr := errors.New("injected promotion cleanup failure")
	auditor := &workspaceAuditRecorder{Auditor: fixture.store}
	service := newWorkspaceAPI(t, fixture, auditor, func(path string) error {
		if strings.HasPrefix(filepath.Base(path), "promotion-") {
			return cleanupErr
		}
		return os.RemoveAll(path)
	})
	value, err := service.CallTool(context.Background(), api.Actor{Identity: "agent@host"}, "promote",
		json.RawMessage(`{"sha":"`+sourceSHA+`","branch":"main"}`))
	if err != nil {
		t.Fatalf("durable promotion was reported as an operation failure: %v", err)
	}
	promotion := requireToolPromotion(t, fixture, value)
	if promotion.Status != model.PromotionPassed || promotion.Error != "" {
		t.Fatalf("promotion outcome was repainted by cleanup failure = %#v", promotion)
	}
	persisted, err := fixture.store.Promotion(context.Background(), promotion.ID)
	if err != nil || persisted.Status != model.PromotionPassed || persisted.Error != "" {
		t.Fatalf("persisted promotion was repainted = %#v, %v", persisted, err)
	}
	if !auditor.recorded("workspace.cleanup.failed", promotion.ID) {
		t.Fatalf("cleanup failure audit actions = %#v", auditor.actions)
	}
	workspace := mustPromotionWorkspacePath(t, filepath.Join(fixture.root, "work"), promotion.ID)
	assertWorkspaceExists(t, workspace)
	if err := fixture.scheduler.recoverOwnedWorkspaces(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertWorkspaceMissing(t, workspace)
}

func TestOwnedWorkspacePathsRejectUnboundedTargets(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"", "../escape", strings.Repeat("a", 31), strings.Repeat("g", 32), strings.Repeat("A", 32)} {
		if path, err := promotionWorkspacePath(root, id); err == nil {
			t.Fatalf("unsafe promotion owner %q resolved to %s", id, path)
		}
	}
	if path, err := runWorkspacePath(string(filepath.Separator), strings.Repeat("a", 32)); err == nil {
		t.Fatalf("filesystem root resolved to %s", path)
	}
}

type workspaceAuditRecorder struct {
	Auditor
	actions []model.AuditActionSpec
}

type blockingCheckoutGit struct {
	*controlGit
	started chan struct{}
	release chan struct{}
	err     error
}

func (git *blockingCheckoutGit) Checkout(_ context.Context, _, _ string, destination string) error {
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return err
	}
	close(git.started)
	<-git.release
	return git.err
}

func (recorder *workspaceAuditRecorder) AppendAuditAction(ctx context.Context, spec model.AuditActionSpec) (model.AuditAction, error) {
	recorder.actions = append(recorder.actions, spec)
	return recorder.Auditor.AppendAuditAction(ctx, spec)
}

func (recorder *workspaceAuditRecorder) recorded(action, resourceID string) bool {
	for _, spec := range recorder.actions {
		if spec.Action == action && spec.ResourceID == resourceID {
			return true
		}
	}
	return false
}

func newWorkspaceAPI(t *testing.T, fixture *controlFixture, auditor Auditor, remove func(string) error) *API {
	t.Helper()
	service, err := NewAPI(APIConfig{
		Runs: fixture.store, History: fixture.store, Repositories: fixture.store,
		Issues: fixture.store, Promotions: fixture.store, PromotionRuns: fixture.store,
		Enqueues: fixture.scheduler, Git: fixture.git, Logs: fixture.logs, Auditor: auditor,
		Signals: fixture.signals, PromotionWorkspaceRoot: filepath.Join(fixture.root, "work"),
		RemoveWorkspace: remove,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func seedWorkspacePromotionCandidate(t *testing.T, fixture *controlFixture, sha string) {
	t.Helper()
	run := enqueueWorkspaceRun(t, fixture, "feature/promotion-source", sha)
	if _, err := fixture.store.ClaimNextRun(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.FinishRun(context.Background(), run.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}
}

func enqueueWorkspaceRun(t *testing.T, fixture *controlFixture, ref, sha string) model.Run {
	t.Helper()
	enqueued, err := fixture.store.EnqueueRun(context.Background(), model.RunSpec{
		RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: ref,
		SHA: sha, Actor: "agent@host", Trigger: "branch",
	})
	if err != nil {
		t.Fatal(err)
	}
	return enqueued.Run
}

func mustPromotionWorkspacePath(t *testing.T, root, promotionID string) string {
	t.Helper()
	path, err := promotionWorkspacePath(root, promotionID)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func assertWorkspaceExists(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		t.Fatalf("workspace %s missing: %v", path, err)
	}
}

func assertWorkspaceMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace %s still exists: %v", path, err)
	}
}
