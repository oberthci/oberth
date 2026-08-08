package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/store"
)

type controlGit struct {
	mu                       sync.Mutex
	publishOnce              sync.Once
	prepareOnce              sync.Once
	promotionPublicationOnce sync.Once
	publishStarted           chan struct{}
	publishRelease           chan struct{}
	promotionPublishStarted  chan struct{}
	promotionPublishRelease  chan struct{}
	promotionPublishHonorCtx bool
	prepareStarted           chan struct{}
	prepareCancel            bool
	plan                     gitcache.MergeCandidate
	prepareErr               error
	checkoutErr              error
	pushErr                  error
	pushMoveSHA              string
	applyThenErr             error
	remoteRefErr             error
	remoteRefs               map[string]string
	checkouts                []string
	syncedBranches           []string
	syncedTags               []string
	promotions               []string
}

func requireToolPromotion(t *testing.T, fixture *controlFixture, value any) model.Promotion {
	t.Helper()
	response, ok := value.(api.PromoteResponse)
	if !ok || response.ID == "" {
		t.Fatalf("promote response = %#v, want only a durable ID", value)
	}
	promotion, err := fixture.store.Promotion(context.Background(), response.ID)
	if err != nil {
		t.Fatalf("load promotion %q: %v", response.ID, err)
	}
	return promotion
}

func (git *controlGit) Checkout(_ context.Context, _ string, sha, destination string) error {
	git.mu.Lock()
	git.checkouts = append(git.checkouts, sha)
	err := git.checkoutErr
	git.mu.Unlock()
	if err != nil {
		return err
	}
	return os.MkdirAll(destination, 0o700)
}

func (git *controlGit) SyncBranch(_ context.Context, repo, branch, sha string) error {
	if git.publishStarted != nil {
		git.publishOnce.Do(func() {
			close(git.publishStarted)
			<-git.publishRelease
		})
	}
	git.mu.Lock()
	defer git.mu.Unlock()
	git.syncedBranches = append(git.syncedBranches, repo+":"+branch+":"+sha)
	git.setRemoteRefLocked("refs/heads/"+branch, sha)
	return git.applyThenErr
}

func TestNewPushWaitsWhilePreviousRunPublishes(t *testing.T) {
	const (
		branch   = "feature/publication-interleaving"
		firstSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		nextSHA  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	fixture := newControlFixture(t,
		JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)}},
		JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)}},
	)
	fixture.git.publishStarted = make(chan struct{})
	fixture.git.publishRelease = make(chan struct{})
	ctx := context.Background()
	first, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "receive-publication-first", Repository: fixture.repo, Branch: branch,
		SHA: firstSHA, Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	processed := make(chan error, 1)
	go func() { processed <- fixture.scheduler.ProcessNext(ctx) }()
	select {
	case <-fixture.git.publishStarted:
	case <-time.After(5 * time.Second):
		close(fixture.git.publishRelease)
		t.Fatal("first run did not reach durable publication")
	}

	replacement, enqueueErr := fixture.scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "receive-publication-next", Repository: fixture.repo, Branch: branch,
		OldSHA: firstSHA, SHA: nextSHA, Actor: "agent@host",
	})
	owner, ownerErr := fixture.store.Run(ctx, first.ID)
	queued, queuedErr := fixture.store.Run(ctx, replacement.ID)
	pending, pendingErr := fixture.store.PendingPublications(ctx)
	close(fixture.git.publishRelease)
	var processErr error
	select {
	case processErr = <-processed:
	case <-time.After(5 * time.Second):
		t.Fatal("first publication did not finish after release")
	}

	if enqueueErr != nil {
		t.Fatal(enqueueErr)
	}
	if ownerErr != nil || owner.Status != model.RunRunning || owner.Phase != "publishing" || owner.SupersededBy != "" {
		t.Fatalf("publication owner during newer enqueue = %#v, %v", owner, ownerErr)
	}
	if queuedErr != nil || queued.Status != model.RunQueued {
		t.Fatalf("newer run while publication pending = %#v, %v", queued, queuedErr)
	}
	if pendingErr != nil || len(pending) != 1 || pending[0].RunID != first.ID {
		t.Fatalf("pending publication during newer enqueue = %#v, %v", pending, pendingErr)
	}
	if processErr != nil {
		t.Fatalf("first publication error = %v", processErr)
	}
	finished, err := fixture.store.Run(ctx, first.ID)
	if err != nil || finished.Status != model.RunPassed {
		t.Fatalf("first terminal run = %#v, %v", finished, err)
	}

	if err := fixture.scheduler.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	finishedNext, err := fixture.store.Run(ctx, replacement.ID)
	if err != nil || finishedNext.Status != model.RunPassed {
		t.Fatalf("newer terminal run = %#v, %v", finishedNext, err)
	}
	remoteSHA, exists, err := fixture.git.RemoteRef(ctx, fixture.repo.Name, "refs/heads/"+branch)
	if err != nil || !exists || remoteSHA != nextSHA {
		t.Fatalf("published branch = %q, %v, %v", remoteSHA, exists, err)
	}
}

func TestSchedulerSkipsSameRefRunWhilePublicationPending(t *testing.T) {
	const (
		branch       = "feature/publication-serialization"
		otherBranch  = "feature/publication-independent"
		firstSHA     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		nextSHA      = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		unrelatedSHA = "cccccccccccccccccccccccccccccccccccccccc"
	)
	fixture := newControlFixture(t,
		JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)}},
		JobResult{Status: model.RunFailed, Phase: "test", FailedBurn: "test", FailedStep: "unit", Error: "independent red", Steps: []model.StepResult{stepResult(model.StepFailed)}},
		JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)}},
	)
	fixture.git.publishStarted = make(chan struct{})
	fixture.git.publishRelease = make(chan struct{})
	var releaseOnce sync.Once
	releasePublication := func() { releaseOnce.Do(func() { close(fixture.git.publishRelease) }) }

	ctx, cancel := context.WithCancel(context.Background())
	var schedulerErr error
	schedulerDone := make(chan struct{})
	go func() {
		schedulerErr = fixture.scheduler.Run(ctx)
		close(schedulerDone)
	}()
	t.Cleanup(func() {
		releasePublication()
		cancel()
		select {
		case <-schedulerDone:
			if schedulerErr != nil {
				t.Errorf("scheduler run: %v", schedulerErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("scheduler did not stop after cancellation")
		}
	})

	waitForStatus := func(runID string, wanted model.RunStatus) model.Run {
		t.Helper()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for {
			run, err := fixture.store.Run(context.Background(), runID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status == wanted {
				return run
			}
			if run.Status.Terminal() {
				t.Fatalf("run %s status = %q, want %q", runID, run.Status, wanted)
			}
			changed := fixture.signals.Run(runID)
			run, err = fixture.store.Run(context.Background(), runID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status == wanted {
				return run
			}
			if run.Status.Terminal() {
				t.Fatalf("run %s status = %q, want %q", runID, run.Status, wanted)
			}
			select {
			case <-changed:
			case <-schedulerDone:
				t.Fatalf("scheduler exited before run %s reached %q: %v", runID, wanted, schedulerErr)
			case <-timer.C:
				t.Fatalf("run %s remained %q, want %q", runID, run.Status, wanted)
			}
		}
	}

	first, err := fixture.scheduler.EnqueueCI(context.Background(), CIRequest{
		EventID: "receive-concurrent-publication-first", Repository: fixture.repo, Branch: branch,
		SHA: firstSHA, Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-fixture.git.publishStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first run did not reach publication")
	}

	replacement, err := fixture.scheduler.EnqueueCI(context.Background(), CIRequest{
		EventID: "receive-concurrent-publication-next", Repository: fixture.repo, Branch: branch,
		OldSHA: firstSHA, SHA: nextSHA, Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	unrelated, err := fixture.scheduler.EnqueueCI(context.Background(), CIRequest{
		EventID: "receive-concurrent-publication-independent", Repository: fixture.repo, Branch: otherBranch,
		SHA: unrelatedSHA, Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(unrelated.ID, model.RunFailed)
	queued, err := fixture.store.Run(context.Background(), replacement.ID)
	if err != nil || queued.Status != model.RunQueued {
		t.Fatalf("same-ref run while first publication is pending = %#v, %v", queued, err)
	}
	owner, err := fixture.store.Run(context.Background(), first.ID)
	if err != nil || owner.Status != model.RunRunning || owner.Phase != "publishing" {
		t.Fatalf("publication owner before release = %#v, %v", owner, err)
	}

	releasePublication()
	waitForStatus(first.ID, model.RunPassed)
	waitForStatus(replacement.ID, model.RunPassed)
	remoteSHA, exists, err := fixture.git.RemoteRef(context.Background(), fixture.repo.Name, "refs/heads/"+branch)
	if err != nil || !exists || remoteSHA != nextSHA {
		t.Fatalf("serialized branch publication = %q, %v, %v", remoteSHA, exists, err)
	}
}

func (git *controlGit) SyncTag(_ context.Context, repo, tag, sha string) error {
	git.mu.Lock()
	defer git.mu.Unlock()
	git.syncedTags = append(git.syncedTags, repo+":"+tag+":"+sha)
	git.setRemoteRefLocked("refs/tags/"+tag, sha)
	return git.applyThenErr
}

func (git *controlGit) PreparePromotion(ctx context.Context, _, _, target string, _ string) (gitcache.MergeCandidate, error) {
	if git.prepareStarted != nil {
		git.prepareOnce.Do(func() { close(git.prepareStarted) })
	}
	if git.prepareCancel {
		<-ctx.Done()
		return gitcache.MergeCandidate{}, ctx.Err()
	}
	git.mu.Lock()
	defer git.mu.Unlock()
	if git.prepareErr == nil && git.plan.BaseSHA != "" {
		git.setRemoteRefLocked("refs/heads/"+target, git.plan.BaseSHA)
	}
	return git.plan, git.prepareErr
}

func (git *controlGit) PushPromotion(ctx context.Context, repo, target, sha string) error {
	if git.promotionPublishStarted != nil {
		git.promotionPublicationOnce.Do(func() {
			close(git.promotionPublishStarted)
			if git.promotionPublishHonorCtx {
				select {
				case <-git.promotionPublishRelease:
				case <-ctx.Done():
				}
			} else {
				<-git.promotionPublishRelease
			}
		})
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	git.mu.Lock()
	defer git.mu.Unlock()
	git.promotions = append(git.promotions, repo+":"+target+":"+sha)
	if git.pushErr != nil {
		if git.pushMoveSHA != "" {
			git.setRemoteRefLocked("refs/heads/"+target, git.pushMoveSHA)
		}
		return git.pushErr
	}
	git.setRemoteRefLocked("refs/heads/"+target, sha)
	return git.applyThenErr
}

func TestFastForwardPromotionOutlivesCanceledRequest(t *testing.T) {
	const (
		sourceSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		baseSHA   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	fixture := newControlFixture(t)
	seedGreenPromotionCandidate(t, fixture, sourceSHA)
	fixture.git.plan = gitcache.MergeCandidate{BaseSHA: baseSHA, MergedSHA: sourceSHA, FastForward: true}
	fixture.git.promotionPublishStarted = make(chan struct{})
	fixture.git.promotionPublishRelease = make(chan struct{})
	fixture.git.promotionPublishHonorCtx = true
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(fixture.git.promotionPublishRelease) }) }
	t.Cleanup(release)

	ctx, cancel := context.WithCancel(context.Background())
	control := fixture.api(t)
	type result struct {
		value any
		err   error
	}
	done := make(chan result, 1)
	go func() {
		value, err := control.CallTool(ctx, api.Actor{Identity: "agent@host"}, "promote",
			json.RawMessage(`{"sha":"`+sourceSHA+`","branch":"main"}`))
		done <- result{value: value, err: err}
	}()
	select {
	case <-fixture.git.promotionPublishStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("fast-forward publication did not start")
	}
	cancel()
	release()
	var completed result
	select {
	case completed = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fast-forward publication did not finish after request cancellation")
	}
	if completed.err != nil {
		t.Fatal(completed.err)
	}
	promotion := requireToolPromotion(t, fixture, completed.value)
	if promotion.Status != model.PromotionPassed {
		t.Fatalf("promotion after caller cancellation = %#v", promotion)
	}
}

func TestFastForwardPendingPublicationRecoversWithoutRestart(t *testing.T) {
	const (
		sourceSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		baseSHA   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	fixture := newControlFixture(t)
	seedGreenPromotionCandidate(t, fixture, sourceSHA)
	fixture.git.plan = gitcache.MergeCandidate{BaseSHA: baseSHA, MergedSHA: sourceSHA, FastForward: true}
	failing := &finalizeFailStore{Store: fixture.store, failNext: true, empty: make(chan struct{}, 8)}
	scheduler, err := NewScheduler(SchedulerConfig{
		Store: failing, Git: fixture.git, Logs: fixture.logs, Jobs: fixture.jobs, ReleaseJobs: fixture.jobs,
		Auditor: fixture.store, Signals: fixture.signals,
		WorkspaceRoot: filepath.Join(fixture.root, "fast-forward-recovery-work"), MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	schedulerCtx, cancelScheduler := context.WithCancel(context.Background())
	schedulerDone := make(chan error, 1)
	go func() { schedulerDone <- scheduler.Run(schedulerCtx) }()
	t.Cleanup(func() {
		cancelScheduler()
		select {
		case runErr := <-schedulerDone:
			if runErr != nil {
				t.Errorf("scheduler run: %v", runErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("scheduler did not stop")
		}
	})
	for index := 0; index < 2; index++ {
		select {
		case <-failing.empty:
		case <-time.After(5 * time.Second):
			t.Fatalf("scheduler startup empty claim %d did not occur", index+1)
		}
	}

	control, err := NewAPI(APIConfig{
		Runs: fixture.store, History: fixture.store, Repositories: fixture.store,
		Issues: fixture.store, Promotions: fixture.store, PromotionRuns: fixture.store,
		Enqueues: scheduler, Git: fixture.git, Logs: fixture.logs, Auditor: fixture.store,
		Signals: fixture.signals, MaximumWait: 50 * time.Millisecond,
		PromotionWorkspaceRoot: filepath.Join(fixture.root, "fast-forward-recovery-api"),
	})
	if err != nil {
		t.Fatal(err)
	}
	value, err := control.CallTool(context.Background(), api.Actor{Identity: "agent@host"}, "promote",
		json.RawMessage(`{"sha":"`+sourceSHA+`","branch":"main"}`))
	if err != nil {
		t.Fatal(err)
	}
	pending := requireToolPromotion(t, fixture, value)
	if pending.Status != model.PromotionPending {
		t.Fatalf("promotion before scheduler recovery = %#v", pending)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		recovered, lookupErr := fixture.store.Promotion(context.Background(), pending.ID)
		if lookupErr != nil {
			t.Fatal(lookupErr)
		}
		if recovered.Status == model.PromotionPassed {
			break
		}
		if recovered.Status.Terminal() {
			t.Fatalf("recovered promotion = %#v", recovered)
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending publication was not recovered without restart: %#v", recovered)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPublicationRecoveryWakeDoesNotSeizeUnrelatedLivePublication(t *testing.T) {
	fixture := newControlFixture(t)
	observed := &claimObservingStore{
		Store: fixture.store, empty: make(chan struct{}, 8), claimed: make(chan model.Run, 8),
	}
	scheduler, err := NewScheduler(SchedulerConfig{
		Store: observed, Git: fixture.git, Logs: fixture.logs, Jobs: fixture.jobs, ReleaseJobs: fixture.jobs,
		Auditor: fixture.store, Signals: fixture.signals,
		WorkspaceRoot: filepath.Join(fixture.root, "targeted-publication-recovery-work"), MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	schedulerCtx, cancelScheduler := context.WithCancel(context.Background())
	schedulerDone := make(chan error, 1)
	go func() { schedulerDone <- scheduler.Run(schedulerCtx) }()
	t.Cleanup(func() {
		cancelScheduler()
		select {
		case runErr := <-schedulerDone:
			if runErr != nil {
				t.Errorf("scheduler run: %v", runErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("scheduler did not stop")
		}
	})
	for index := 0; index < 2; index++ {
		select {
		case <-observed.empty:
		case <-time.After(5 * time.Second):
			t.Fatalf("scheduler startup empty claim %d did not occur", index+1)
		}
	}

	beginPending := func(branch, sha string) (model.Run, model.Publication) {
		t.Helper()
		run, enqueueErr := fixture.store.EnqueueRun(context.Background(), model.RunSpec{
			RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: branch,
			SHA: sha, Actor: "agent@host", Trigger: "branch", TestedSHA: sha,
		})
		if enqueueErr != nil {
			t.Fatal(enqueueErr)
		}
		claimed, claimErr := fixture.store.ClaimNextRun(context.Background())
		if claimErr != nil {
			t.Fatal(claimErr)
		}
		if claimed.ID != run.ID {
			t.Fatalf("claimed run = %s, want %s", claimed.ID, run.ID)
		}
		publication, beginErr := fixture.store.BeginPublication(context.Background(), model.PublicationSpec{
			RepoID: fixture.repo.ID, RunID: run.ID, RefKind: model.RefBranch, Ref: branch,
			ResultSHA: sha, Actor: run.Actor,
		})
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		return claimed, publication
	}
	unrelatedRun, _ := beginPending("feature/api-owned", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	recoveryRun, recoveryPublication := beginPending("feature/recover-exact", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	scheduler.NotifyPublicationRecovery(recoveryPublication.ID)
	waitForFixtureRunStatus(t, fixture, recoveryRun.ID, model.RunPassed)
	stillOwned, err := fixture.store.Run(context.Background(), unrelatedRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillOwned.Status != model.RunRunning || stillOwned.Phase != "publishing" {
		t.Fatalf("unrelated live publication was seized by recovery: %#v", stillOwned)
	}
}

func TestPublicationCoordinatorSerializesLiveDeliveryAndStartupRecovery(t *testing.T) {
	const (
		baseSHA   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		resultSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	fixture := newControlFixture(t)
	_, publication := beginPendingPromotionPublication(t, fixture, "main", baseSHA, resultSHA)
	fixture.git.setRemoteRefLocked("refs/heads/main", baseSHA)
	fixture.git.promotionPublishStarted = make(chan struct{})
	fixture.git.promotionPublishRelease = make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-fixture.git.promotionPublishRelease:
		default:
			close(fixture.git.promotionPublishRelease)
		}
	})

	type result struct {
		finalization model.PublicationFinalization
		err          error
	}
	deliveryDone := make(chan result, 1)
	go func() {
		finalization, err := fixture.scheduler.DeliverPublication(context.Background(), publication.ID)
		deliveryDone <- result{finalization: finalization, err: err}
	}()
	select {
	case <-fixture.git.promotionPublishStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("live publication delivery did not start")
	}
	recoveryDone := make(chan error, 1)
	go func() { recoveryDone <- fixture.scheduler.recoverPendingPublications(context.Background()) }()
	select {
	case err := <-recoveryDone:
		t.Fatalf("startup recovery bypassed active delivery coordinator: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(fixture.git.promotionPublishRelease)

	select {
	case delivered := <-deliveryDone:
		if delivered.err != nil || delivered.finalization.Promotion.Status != model.PromotionPassed {
			t.Fatalf("live delivery = %#v, %v", delivered.finalization, delivered.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("live publication delivery did not finish")
	}
	select {
	case err := <-recoveryDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("startup recovery did not observe terminal publication")
	}
	if len(fixture.git.promotions) != 1 {
		t.Fatalf("publication mutations = %#v, want exactly one", fixture.git.promotions)
	}
}

func TestPublicationCoordinatorRechecksGateAfterBoundedWait(t *testing.T) {
	const (
		baseSHA   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		resultSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	t.Run("gate closes while waiting", func(t *testing.T) {
		fixture := newControlFixture(t)
		gate := &mutableMutationGate{}
		scheduler, err := NewScheduler(SchedulerConfig{
			Store: fixture.store, Git: fixture.git, Logs: fixture.logs, Jobs: fixture.jobs, ReleaseJobs: fixture.jobs,
			Auditor: fixture.store, Signals: fixture.signals,
			WorkspaceRoot: filepath.Join(fixture.root, "publication-gate-recheck-work"), MaxConcurrent: 1,
			MutationGate: gate.check,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, publication := beginPendingPromotionPublication(t, fixture, "main", baseSHA, resultSHA)
		permit := <-scheduler.deliveryPermit
		done := make(chan error, 1)
		started := make(chan struct{})
		go func() {
			close(started)
			_, deliveryErr := scheduler.DeliverPublication(context.Background(), publication.ID)
			done <- deliveryErr
		}()
		<-started
		gate.set(errors.New("audit witness unavailable"))
		scheduler.deliveryPermit <- permit
		select {
		case deliveryErr := <-done:
			if !errors.Is(deliveryErr, errMutationBlocked) {
				t.Fatalf("delivery after gate closed = %v, want mutation blocked", deliveryErr)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("delivery remained blocked after coordinator release")
		}
		if len(fixture.git.promotions) != 0 {
			t.Fatalf("Git mutated after gate closed: %#v", fixture.git.promotions)
		}
	})

	t.Run("deadline while waiting", func(t *testing.T) {
		fixture := newControlFixture(t)
		_, publication := beginPendingPromotionPublication(t, fixture, "main", baseSHA, resultSHA)
		permit := <-fixture.scheduler.deliveryPermit
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		defer cancel()
		_, err := fixture.scheduler.DeliverPublication(ctx, publication.ID)
		fixture.scheduler.deliveryPermit <- permit
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("delivery waiting past deadline = %v, want deadline exceeded", err)
		}
		if len(fixture.git.promotions) != 0 {
			t.Fatalf("timed-out delivery mutated Git: %#v", fixture.git.promotions)
		}
	})
}

func beginPendingPromotionPublication(
	t *testing.T,
	fixture *controlFixture,
	target string,
	baseSHA string,
	resultSHA string,
) (model.Promotion, model.Publication) {
	t.Helper()
	promotion, err := fixture.store.AppendPromotion(context.Background(), model.PromotionSpec{
		RepoID: fixture.repo.ID, SourceBranch: "feature/publication-coordinator",
		SourceSHA: resultSHA, TargetRef: target, Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	promotion, err = fixture.store.PlanPromotion(context.Background(), promotion.ID, baseSHA, resultSHA)
	if err != nil {
		t.Fatal(err)
	}
	publication, err := fixture.store.BeginPublication(context.Background(), model.PublicationSpec{
		RepoID: fixture.repo.ID, PromotionID: promotion.ID, RefKind: model.RefBranch, Ref: target,
		PreviousSHA: baseSHA, ResultSHA: resultSHA, Actor: promotion.Actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	return promotion, publication
}

func seedGreenPromotionCandidate(t *testing.T, fixture *controlFixture, sha string) {
	t.Helper()
	run, err := fixture.store.EnqueueRun(context.Background(), model.RunSpec{
		RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: "feature/promote",
		SHA: sha, Actor: "agent@host", Trigger: "branch", TestedSHA: sha,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.ClaimNextRun(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.FinishRun(context.Background(), run.ID, model.RunResult{
		Status: model.RunPassed, TestedSHA: sha,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerPausesAndResumesCompletedWorkWhenMutationGateCloses(t *testing.T) {
	fixture := newControlFixture(t)
	result := JobResult{
		Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)},
	}
	jobs := &completingJobs{started: make(chan struct{}), release: make(chan struct{}), result: result}
	gate := &mutableMutationGate{}
	scheduler, err := NewScheduler(SchedulerConfig{
		Store: fixture.store, Git: fixture.git, Logs: fixture.logs, Jobs: jobs, ReleaseJobs: jobs,
		Auditor: fixture.store, Signals: fixture.signals,
		WorkspaceRoot: filepath.Join(fixture.root, "mutation-pause-work"), MaxConcurrent: 1,
		MutationGate: gate.check,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := fixture.store.EnqueueRun(context.Background(), model.RunSpec{
		RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: "feature/audit-pause",
		SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actor: "agent@host", Trigger: "branch",
		TestedSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	})
	if err != nil {
		t.Fatal(err)
	}
	schedulerCtx, cancelScheduler := context.WithCancel(context.Background())
	var schedulerErr error
	schedulerDone := make(chan struct{})
	go func() {
		schedulerErr = scheduler.Run(schedulerCtx)
		close(schedulerDone)
	}()
	defer func() {
		cancelScheduler()
		select {
		case <-schedulerDone:
		case <-time.After(5 * time.Second):
			t.Error("scheduler did not stop")
		}
	}()
	select {
	case <-jobs.started:
	case <-time.After(5 * time.Second):
		t.Fatal("Job did not start")
	}
	gate.set(errors.New("audit witness unavailable"))
	close(jobs.release)
	select {
	case <-schedulerDone:
		t.Fatalf("scheduler exited during recoverable audit outage: %v", schedulerErr)
	case <-time.After(200 * time.Millisecond):
	}
	paused, err := fixture.store.Run(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paused.Status != model.RunRunning {
		t.Fatalf("run while mutation gate closed = %#v", paused)
	}

	gate.set(nil)
	finished := waitForFixtureRunStatus(t, fixture, run.ID, model.RunPassed)
	if finished.Status != model.RunPassed {
		t.Fatalf("run after mutation gate recovery = %#v", finished)
	}
	cancelScheduler()
	select {
	case <-schedulerDone:
		if schedulerErr != nil {
			t.Fatalf("scheduler after recovery = %v", schedulerErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not stop after recovered run")
	}
}

func TestDurableEnqueueWakeSurvivesMutationGateClosingBeforeAcceptance(t *testing.T) {
	fixture := newControlFixture(t, JobResult{
		Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)},
	})
	gate := &mutableMutationGate{}
	observed := &gateClosingEnqueueStore{Store: fixture.store, gate: gate, empty: make(chan struct{}, 8)}
	scheduler, err := NewScheduler(SchedulerConfig{
		Store: observed, Git: fixture.git, Logs: fixture.logs, Jobs: fixture.jobs, ReleaseJobs: fixture.jobs,
		Auditor: fixture.store, Signals: fixture.signals,
		WorkspaceRoot: filepath.Join(fixture.root, "durable-enqueue-wake-work"), MaxConcurrent: 1,
		MutationGate: gate.check,
	})
	if err != nil {
		t.Fatal(err)
	}
	schedulerCtx, cancelScheduler := context.WithCancel(context.Background())
	schedulerDone := make(chan error, 1)
	go func() { schedulerDone <- scheduler.Run(schedulerCtx) }()
	t.Cleanup(func() {
		cancelScheduler()
		select {
		case runErr := <-schedulerDone:
			if runErr != nil {
				t.Errorf("scheduler run: %v", runErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("scheduler did not stop")
		}
	})
	for index := 0; index < 2; index++ {
		select {
		case <-observed.empty:
		case <-time.After(5 * time.Second):
			t.Fatalf("scheduler startup empty claim %d did not occur", index+1)
		}
	}

	enqueued, err := scheduler.EnqueueCI(context.Background(), CIRequest{
		EventID: "receive-gate-closes-after-enqueue", Repository: fixture.repo,
		Branch: "feature/durable-wake", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Actor: "agent@host",
	})
	if !errors.Is(err, errMutationBlocked) {
		t.Fatalf("enqueue error = %v, want mutation blocked after durable admission", err)
	}
	gate.set(nil)
	waitForFixtureRunStatus(t, fixture, enqueued.ID, model.RunPassed)
}

func (git *controlGit) RemoteRef(_ context.Context, _ string, ref string) (string, bool, error) {
	git.mu.Lock()
	defer git.mu.Unlock()
	if git.remoteRefErr != nil {
		return "", false, git.remoteRefErr
	}
	sha := git.remoteRefLocked(ref)
	return sha, sha != "", nil
}

func (git *controlGit) remoteRefLocked(ref string) string {
	if git.remoteRefs == nil {
		return ""
	}
	return git.remoteRefs[ref]
}

func (git *controlGit) setRemoteRefLocked(ref, sha string) {
	if git.remoteRefs == nil {
		git.remoteRefs = make(map[string]string)
	}
	git.remoteRefs[ref] = sha
}

type fakeJobs struct {
	mu              sync.Mutex
	results         []JobResult
	waitErr         error
	createdCI       []JobRequest
	createdReleases []JobRequest
	deleted         []string
}

type blockingJobs struct {
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
	deleted  chan string
}

type completingJobs struct {
	started chan struct{}
	release chan struct{}
	result  JobResult
	once    sync.Once
}

func (*completingJobs) CreateCI(context.Context, JobRequest) error      { return nil }
func (*completingJobs) CreateRelease(context.Context, JobRequest) error { return nil }
func (jobs *completingJobs) Wait(_ context.Context, _ string, output io.Writer) (JobResult, error) {
	jobs.once.Do(func() { close(jobs.started) })
	<-jobs.release
	for _, step := range jobs.result.Steps {
		_, _ = fmt.Fprintf(output, "[%s/%s] output for %s\n", step.Burn, step.Step, step.Status)
	}
	return jobs.result, nil
}
func (*completingJobs) Delete(context.Context, string, string) error { return nil }

type mutableMutationGate struct {
	mu  sync.RWMutex
	err error
}

func (gate *mutableMutationGate) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	gate.mu.RLock()
	defer gate.mu.RUnlock()
	return gate.err
}

func (gate *mutableMutationGate) set(err error) {
	gate.mu.Lock()
	gate.err = err
	gate.mu.Unlock()
}

type createCancellationRaceJobs struct {
	mu             sync.Mutex
	createStarted  chan struct{}
	allowCreate    chan struct{}
	created        bool
	deleteBefore   int
	deleteAfter    int
	deleteFailures int
}

func (jobs *createCancellationRaceJobs) CreateCI(context.Context, JobRequest) error {
	close(jobs.createStarted)
	<-jobs.allowCreate
	jobs.mu.Lock()
	jobs.created = true
	jobs.mu.Unlock()
	return nil
}

func (*createCancellationRaceJobs) CreateRelease(context.Context, JobRequest) error { return nil }

func (*createCancellationRaceJobs) Wait(context.Context, string, io.Writer) (JobResult, error) {
	return JobResult{}, errors.New("superseded Job unexpectedly reached Wait")
}

func (jobs *createCancellationRaceJobs) Delete(context.Context, string, string) error {
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	if jobs.created {
		jobs.deleteAfter++
	} else {
		jobs.deleteBefore++
	}
	if jobs.deleteFailures > 0 {
		jobs.deleteFailures--
		return errors.New("injected Job deletion failure")
	}
	jobs.created = false
	return nil
}

type runReadFailStore struct {
	*store.Store
	failNextRun bool
}

func (value *runReadFailStore) Run(ctx context.Context, id string) (model.Run, error) {
	if value.failNextRun {
		value.failNextRun = false
		return model.Run{}, errors.New("injected post-create Run read failure")
	}
	return value.Store.Run(ctx, id)
}

type flakyDeleteJobs struct {
	*fakeJobs
	deleteFailures int
	attempted      []string
}

func (jobs *flakyDeleteJobs) Delete(_ context.Context, name, _ string) error {
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	jobs.attempted = append(jobs.attempted, name)
	if jobs.deleteFailures > 0 {
		jobs.deleteFailures--
		return errors.New("injected Job deletion failure")
	}
	jobs.deleted = append(jobs.deleted, name)
	return nil
}

func newBlockingJobs() *blockingJobs {
	return &blockingJobs{
		started:  make(chan struct{}),
		canceled: make(chan struct{}),
		release:  make(chan struct{}),
		deleted:  make(chan string, 1),
	}
}

func (*blockingJobs) CreateCI(context.Context, JobRequest) error { return nil }

func (*blockingJobs) CreateRelease(context.Context, JobRequest) error { return nil }

func (jobs *blockingJobs) Wait(ctx context.Context, _ string, _ io.Writer) (JobResult, error) {
	close(jobs.started)
	<-ctx.Done()
	close(jobs.canceled)
	<-jobs.release
	return JobResult{}, ctx.Err()
}

func (jobs *blockingJobs) Delete(_ context.Context, name, _ string) error {
	jobs.deleted <- name
	return nil
}

func (jobs *fakeJobs) CreateCI(_ context.Context, request JobRequest) error {
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	jobs.createdCI = append(jobs.createdCI, request)
	return nil
}

func (jobs *fakeJobs) CreateRelease(_ context.Context, request JobRequest) error {
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	jobs.createdReleases = append(jobs.createdReleases, request)
	return nil
}

func (jobs *fakeJobs) Wait(_ context.Context, _ string, output io.Writer) (JobResult, error) {
	jobs.mu.Lock()
	if jobs.waitErr != nil {
		err := jobs.waitErr
		jobs.mu.Unlock()
		return JobResult{}, err
	}
	if len(jobs.results) == 0 {
		jobs.mu.Unlock()
		return JobResult{}, errors.New("no fake Job result")
	}
	result := jobs.results[0]
	jobs.results = jobs.results[1:]
	jobs.mu.Unlock()
	for _, step := range result.Steps {
		_, _ = fmt.Fprintf(output, "[%s/%s] output for %s\n", step.Burn, step.Step, step.Status)
	}
	return result, nil
}

func (jobs *fakeJobs) Delete(_ context.Context, name, _ string) error {
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	jobs.deleted = append(jobs.deleted, name)
	return nil
}

type controlFixture struct {
	store     *store.Store
	repo      model.Repository
	logs      *runlog.Store
	git       *controlGit
	refs      RefResolver
	jobs      *fakeJobs
	scheduler *Scheduler
	signals   *Signals
	root      string
}

// stubRefResolver maps repo -> branch -> sha for testing.
type stubRefResolver struct {
	branches map[string]map[string]string
}

func (s stubRefResolver) RefSHA(_ context.Context, repo, branch string) (string, error) {
	if branches, ok := s.branches[repo]; ok {
		if sha, ok := branches[branch]; ok {
			return sha, nil
		}
	}
	return "", fmt.Errorf("branch %s not found in %s", branch, repo)
}

type finalizeFailStore struct {
	*store.Store
	failNext bool
	empty    chan struct{}
}

type claimObservingStore struct {
	*store.Store
	empty   chan struct{}
	claimed chan model.Run
}

type gateClosingEnqueueStore struct {
	*store.Store
	gate  *mutableMutationGate
	empty chan struct{}
	once  sync.Once
}

func (value *gateClosingEnqueueStore) EnqueueReceiveEvent(
	ctx context.Context,
	event model.ReceiveEventSpec,
	run model.RunSpec,
) (model.EnqueueRunResult, error) {
	enqueued, err := value.Store.EnqueueReceiveEvent(ctx, event, run)
	if err == nil {
		value.once.Do(func() { value.gate.set(errors.New("audit witness unavailable")) })
	}
	return enqueued, err
}

func (value *gateClosingEnqueueStore) ClaimNextRun(ctx context.Context) (model.Run, error) {
	run, err := value.Store.ClaimNextRun(ctx)
	if queueEmpty(err) {
		select {
		case value.empty <- struct{}{}:
		default:
		}
	}
	return run, err
}

func (value *claimObservingStore) ClaimNextRun(ctx context.Context) (model.Run, error) {
	run, err := value.Store.ClaimNextRun(ctx)
	switch {
	case err == nil:
		select {
		case value.claimed <- run:
		default:
		}
	case queueEmpty(err):
		select {
		case value.empty <- struct{}{}:
		default:
		}
	}
	return run, err
}

type promotionAuditFailer struct {
	*store.Store
	err error
}

type blockingPromotionAuditor struct {
	*store.Store
	started    chan struct{}
	release    chan struct{}
	waitCancel bool
	err        error
	once       sync.Once
}

func (value *blockingPromotionAuditor) AppendAuditAction(ctx context.Context, spec model.AuditActionSpec) (model.AuditAction, error) {
	if strings.HasPrefix(spec.Action, "promotion.") && spec.Action != "promotion.push" {
		value.once.Do(func() { close(value.started) })
		if value.waitCancel {
			<-ctx.Done()
			return model.AuditAction{}, ctx.Err()
		}
		select {
		case <-value.release:
			return model.AuditAction{}, value.err
		case <-ctx.Done():
			return model.AuditAction{}, ctx.Err()
		}
	}
	return value.Store.AppendAuditAction(ctx, spec)
}

type cancelingEnqueueObserver struct {
	started chan struct{}
	once    sync.Once
}

func (value *cancelingEnqueueObserver) AcceptEnqueue(ctx context.Context, _ model.EnqueueRunResult) error {
	value.once.Do(func() { close(value.started) })
	<-ctx.Done()
	return ctx.Err()
}

func (*cancelingEnqueueObserver) NotifyQueue() {}
func (*cancelingEnqueueObserver) DeliverPublication(context.Context, string) (model.PublicationFinalization, error) {
	return model.PublicationFinalization{}, errors.New("publication delivery is unavailable")
}
func (*cancelingEnqueueObserver) NotifyPublicationRecovery(string) {}

func (value *promotionAuditFailer) AppendAuditAction(ctx context.Context, spec model.AuditActionSpec) (model.AuditAction, error) {
	if strings.HasPrefix(spec.Action, "promotion.") && spec.Action != "promotion.push" {
		return model.AuditAction{}, value.err
	}
	return value.Store.AppendAuditAction(ctx, spec)
}

type issueListFailer struct {
	*store.Store
	err error
}

func (value *issueListFailer) ListIssues(context.Context, model.IssueListFilter) (model.IssuePage, error) {
	return model.IssuePage{}, value.err
}

type issueRenewFailer struct {
	*store.Store
	err   error
	calls int
}

func (value *issueRenewFailer) RenewIssueLock(context.Context, int64, string) (model.IssueLock, error) {
	value.calls++
	return model.IssueLock{}, value.err
}

type issueCloseInterleaver struct {
	*store.Store
	replacement model.Issue
}

func (value *issueCloseInterleaver) Issue(ctx context.Context, id int64) (model.Issue, error) {
	issue, err := value.Store.Issue(ctx, id)
	if err != nil || issue.Kind != model.IssueCI || value.replacement.ID != 0 {
		return issue, err
	}
	if _, err := value.CloseCIIssue(ctx, "runner@oberth", issue.ID, "interleaving close"); err != nil {
		return model.Issue{}, err
	}
	value.replacement, err = value.UpsertCIIssue(
		ctx, "runner@oberth", issue.RepoID, issue.Branch, "replacement failure", "new incident",
	)
	return issue, err
}

func (value *finalizeFailStore) FinalizePublication(ctx context.Context, id string, status model.PublicationStatus, failure string) (model.PublicationFinalization, error) {
	if value.failNext {
		value.failNext = false
		return model.PublicationFinalization{}, errors.New("injected crash before publication finalization")
	}
	return value.Store.FinalizePublication(ctx, id, status, failure)
}

func (value *finalizeFailStore) ClaimNextRun(ctx context.Context) (model.Run, error) {
	run, err := value.Store.ClaimNextRun(ctx)
	if queueEmpty(err) && value.empty != nil {
		select {
		case value.empty <- struct{}{}:
		default:
		}
	}
	return run, err
}

func newControlFixture(t *testing.T, results ...JobResult) *controlFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(root, "oberth.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/acme"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := database.CreateRepository(ctx, model.RepositorySpec{Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	logs, err := runlog.Open(filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	git := &controlGit{}
	jobs := &fakeJobs{results: append([]JobResult(nil), results...)}
	signals := NewSignals()
	scheduler, err := NewScheduler(SchedulerConfig{
		Store: database, Git: git, Logs: logs, Jobs: jobs, ReleaseJobs: jobs,
		Auditor: database, Signals: signals, WorkspaceRoot: filepath.Join(root, "work"), MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &controlFixture{store: database, repo: repository, logs: logs, git: git, jobs: jobs, scheduler: scheduler, signals: signals, root: root}
}

func (fixture *controlFixture) api(t *testing.T) *API {
	t.Helper()
	service, err := NewAPI(APIConfig{
		Runs: fixture.store, History: fixture.store, Repositories: fixture.store,
		Issues: fixture.store, Promotions: fixture.store, PromotionRuns: fixture.store,
		Enqueues: fixture.scheduler, Git: fixture.git, Refs: fixture.refs,
		Logs: fixture.logs, Auditor: fixture.store,
		Signals: fixture.signals, MaximumWait: 50 * time.Millisecond,
		PromotionWorkspaceRoot: filepath.Join(fixture.root, "promotion-work"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func stepResult(status model.StepStatus) model.StepResult {
	return model.StepResult{Burn: "test", Step: "unit", Status: status, ExitCode: map[bool]int{true: 0, false: 1}[status == model.StepPassed]}
}

func waitForFixtureRunStatus(t *testing.T, fixture *controlFixture, runID string, wanted model.RunStatus) model.Run {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		run, err := fixture.store.Run(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == wanted {
			return run
		}
		if run.Status.Terminal() {
			t.Fatalf("run %s status = %q, want %q", runID, run.Status, wanted)
		}
		changed := fixture.signals.Run(runID)
		run, err = fixture.store.Run(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == wanted {
			return run
		}
		if run.Status.Terminal() {
			t.Fatalf("run %s status = %q, want %q", runID, run.Status, wanted)
		}
		select {
		case <-changed:
		case <-timer.C:
			t.Fatalf("run %s remained %q, want %q", runID, run.Status, wanted)
		}
	}
}

func TestSchedulerUpdatesOneCIIssueAcrossRedRedGreen(t *testing.T) {
	fixture := newControlFixture(t,
		JobResult{Status: model.RunFailed, Phase: "test", FailedBurn: "test", FailedStep: "unit", Error: "first red", Steps: []model.StepResult{stepResult(model.StepFailed)}},
		JobResult{Status: model.RunFailed, Phase: "test", FailedBurn: "test", FailedStep: "unit", Error: "second red", Steps: []model.StepResult{stepResult(model.StepFailed)}},
		JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)}},
	)
	ctx := context.Background()
	shas := []string{
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"cccccccccccccccccccccccccccccccccccccccc",
	}
	var issueID int64
	for index, sha := range shas {
		if _, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{
			EventID: fmt.Sprintf("receive-%d", index), Repository: fixture.repo,
			Branch: "feature/red-green", SHA: sha, Actor: "agent@host",
		}); err != nil {
			t.Fatal(err)
		}
		if err := fixture.scheduler.ProcessNext(ctx); err != nil {
			t.Fatal(err)
		}
		page, err := fixture.store.ListIssues(ctx, model.IssueListFilter{RepoID: fixture.repo.ID})
		if err != nil || len(page.Issues) != 1 {
			t.Fatalf("issues after run %d = %#v, %v", index, page, err)
		}
		if index == 0 {
			issueID = page.Issues[0].ID
		}
		if page.Issues[0].ID != issueID {
			t.Fatalf("CI issue ID changed from %d to %d", issueID, page.Issues[0].ID)
		}
		if index < 2 && (page.Issues[0].State != model.IssueOpen || page.Issues[0].Occurrences != int64(index+1)) {
			t.Fatalf("red issue after run %d = %#v", index, page.Issues[0])
		}
		if index < 2 {
			if !strings.Contains(page.Issues[0].Body, sha) ||
				!strings.Contains(page.Issues[0].Body, "[test/unit] output for failed") ||
				!strings.Contains(page.Issues[0].Body, "full step log: logs "+sha+" unit") ||
				(index == 1 && strings.Contains(page.Issues[0].Body, shas[0])) {
				t.Fatalf("red issue did not replace its SHA, bounded tail, and log hint: %#v", page.Issues[0])
			}
			if _, err := fixture.store.AcquireIssueLock(ctx, issueID, "monitor@host"); !errors.Is(err, store.ErrLockHeld) {
				t.Fatalf("red issue lock error = %v, want ErrLockHeld", err)
			}
		}
		if index == 2 && (page.Issues[0].State != model.IssueClosed || !strings.Contains(page.Issues[0].Body, "resolved by "+sha+" (run ")) {
			t.Fatalf("green did not close issue: %#v", page.Issues[0])
		}
		value, err := fixture.api(t).CallTool(ctx, api.Actor{Identity: "agent@host"}, "status", json.RawMessage(`{"ref":"feature/red-green"}`))
		if err != nil {
			t.Fatal(err)
		}
		status := value.(StatusResponse)
		if status.Repo != "oberth" || status.Ref != "feature/red-green" || status.SHA != sha || status.RunID == "" || status.Burns["test"] == "" {
			t.Fatalf("wire status after run %d = %#v", index, status)
		}
		if index < 2 && (status.Status != "failed" || status.FailedStep != "unit" || status.ExitCode == nil || *status.ExitCode != 1 || status.Issue == nil || *status.Issue != issueID || status.Burns["test"] != "failed") {
			t.Fatalf("red wire status = %#v", status)
		}
		if index == 2 && (status.Status != "delivered" || status.Issue != nil || status.Burns["test"] != "passed") {
			t.Fatalf("green wire status = %#v", status)
		}
		encoded, err := json.Marshal(status)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), `"repository"`) || strings.Contains(string(encoded), `"steps"`) {
			t.Fatalf("status leaked internal model: %s", encoded)
		}
	}
	if len(fixture.git.syncedBranches) != 1 || fixture.git.syncedBranches[0] != "oberth:feature/red-green:"+shas[2] {
		t.Fatalf("green branch syncs = %#v", fixture.git.syncedBranches)
	}
}

func TestStatusReturnsRefWithoutRunWhenBranchExists(t *testing.T) {
	const branchSHA = "dddddddddddddddddddddddddddddddddddddddd"
	fixture := newControlFixture(t)
	fixture.refs = stubRefResolver{branches: map[string]map[string]string{
		"oberth": {"feature/no-runs": branchSHA},
	}}
	ctx := context.Background()

	// (a) Branch with no runs returns structured no-runs response.
	value, err := fixture.api(t).CallTool(ctx, api.Actor{Identity: "agent@host"}, "status",
		json.RawMessage(`{"ref":"feature/no-runs"}`))
	if err != nil {
		t.Fatalf("status for branch with no runs: %v", err)
	}
	status := value.(StatusResponse)
	if status.Repo != "oberth" || status.Ref != "feature/no-runs" || status.SHA != branchSHA ||
		status.Status != "no-runs" || status.RunID != "" || len(status.Burns) != 0 {
		t.Fatalf("no-runs response = %#v", status)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	wire := string(encoded)
	if !strings.Contains(wire, `"status":"no-runs"`) || !strings.Contains(wire, `"sha":"`+branchSHA+`"`) ||
		!strings.Contains(wire, `"repo":"oberth"`) || !strings.Contains(wire, `"ref":"feature/no-runs"`) ||
		!strings.Contains(wire, `"run":""`) || strings.Contains(wire, `"repository"`) {
		t.Fatalf("no-runs wire JSON = %s", wire)
	}

	// (b) Unknown selector still returns not-found.
	_, err = fixture.api(t).CallTool(ctx, api.Actor{Identity: "agent@host"}, "status",
		json.RawMessage(`{"ref":"does-not-exist"}`))
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown selector error = %v, want not-found", err)
	}

	// (c) Existing run resolution is unchanged.
	const runSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "receive-run", Repository: fixture.repo,
		Branch: "feature/has-run", SHA: runSHA, Actor: "agent@host",
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.scheduler.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	value, err = fixture.api(t).CallTool(ctx, api.Actor{Identity: "agent@host"}, "status",
		json.RawMessage(`{"ref":"feature/has-run"}`))
	if err != nil {
		t.Fatalf("status for branch with run: %v", err)
	}
	runStatus := value.(StatusResponse)
	if runStatus.Repo != "oberth" || runStatus.Ref != "feature/has-run" || runStatus.SHA != runSHA ||
		runStatus.RunID == "" || runStatus.Status == "no-runs" {
		t.Fatalf("run status response = %#v", runStatus)
	}
}

func TestStatusRefWithoutRunRespectsRepoDisambiguator(t *testing.T) {
	const branchSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	fixture := newControlFixture(t)
	fixture.refs = stubRefResolver{branches: map[string]map[string]string{
		"oberth": {"shared-branch": branchSHA},
	}}
	ctx := context.Background()

	// With explicit repo, resolves correctly.
	value, err := fixture.api(t).CallTool(ctx, api.Actor{Identity: "agent@host"}, "status",
		json.RawMessage(`{"repo":"oberth","ref":"shared-branch"}`))
	if err != nil {
		t.Fatalf("disambiguated status: %v", err)
	}
	status := value.(StatusResponse)
	if status.Repo != "oberth" || status.SHA != branchSHA || status.Status != "no-runs" {
		t.Fatalf("disambiguated no-runs = %#v", status)
	}
}

func TestLogsReturnOnlyOneNamedStep(t *testing.T) {
	fixture := newControlFixture(t, JobResult{
		Status: model.RunFailed, Phase: "test", FailedBurn: "test", FailedStep: "unit", Error: "unit failed",
		Steps: []model.StepResult{
			{Burn: "lint", Step: "vet", Status: model.StepPassed, ExitCode: 0},
			{Burn: "test", Step: "setup", Status: model.StepPassed, ExitCode: 0},
			{Burn: "test", Step: "unit", Status: model.StepFailed, ExitCode: 1},
		},
	})
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ctx := context.Background()
	if _, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "receive-burn-logs", Repository: fixture.repo,
		Branch: "feature/burn-logs", SHA: sha, Actor: "agent@host",
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.scheduler.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	value, err := fixture.api(t).CallTool(ctx, api.Actor{Identity: "agent@host"}, "logs",
		json.RawMessage(`{"sha":"`+sha+`","step":"unit"}`))
	if err != nil {
		t.Fatal(err)
	}
	response := value.(LogResponse)
	if response.Burn != "test" || response.Step != "unit" ||
		strings.Contains(response.Output, "[test/setup]") ||
		!strings.Contains(response.Output, "[test/unit]") ||
		strings.Contains(response.Output, "[lint/vet]") {
		t.Fatalf("step log response = %#v", response)
	}
}

func TestDashboardRunDetailAndLogViewsAreReadOnly(t *testing.T) {
	fixture := newControlFixture(t, JobResult{
		Status: model.RunFailed, Phase: "test", FailedBurn: "test", FailedStep: "unit", Error: "unit failed",
		Steps: []model.StepResult{
			{Burn: "lint", Step: "vet", Status: model.StepPassed, ExitCode: 0},
			{Burn: "test", Step: "unit", Status: model.StepFailed, ExitCode: 1},
		},
	})
	const sha = "abababababababababababababababababababab"
	ctx := context.Background()
	enqueued, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "receive-dashboard-detail", Repository: fixture.repo,
		Branch: "feature/dashboard-detail", SHA: sha, Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.scheduler.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	control := fixture.api(t)

	value, err := control.Run(ctx, api.Actor{Identity: "agent@host"}, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	detail := value.(RunDetailResponse)
	if detail.Run.ID != enqueued.ID || detail.Run.Status != model.RunFailed || detail.Repository.Name != fixture.repo.Name {
		t.Fatalf("run detail = %#v", detail)
	}
	if len(detail.Steps) != 2 || detail.Steps[0].Burn != "lint" || detail.Steps[1].Step != "unit" || detail.Steps[1].Status != model.StepFailed {
		t.Fatalf("run detail steps = %#v", detail.Steps)
	}
	// The red run's CI issue stays locked for the pushing actor exactly as the
	// scheduler left it: the read-only view neither stole, released, nor
	// renewed that lock, and the dashboard viewer identity owns nothing.
	issue, err := fixture.store.OpenCIIssue(ctx, fixture.repo.ID, "feature/dashboard-detail")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.AcquireIssueLock(ctx, issue.ID, "another@agent"); !errors.Is(err, store.ErrLockHeld) {
		t.Fatalf("pusher's CI issue lock was disturbed by the dashboard view: %v", err)
	}
	if _, err := fixture.store.RenewIssueLock(ctx, issue.ID, "dashboard@viewer"); !errors.Is(err, store.ErrLockNotOwned) {
		t.Fatalf("dashboard viewer unexpectedly holds issue coordination state: %v", err)
	}

	logValue, err := control.RunLog(ctx, api.Actor{Identity: "agent@host"}, enqueued.ID, "test", "unit")
	if err != nil {
		t.Fatal(err)
	}
	logResponse := logValue.(LogResponse)
	if logResponse.RunID != enqueued.ID || !strings.Contains(logResponse.Output, "[test/unit]") || strings.Contains(logResponse.Output, "[lint/vet]") {
		t.Fatalf("dashboard log response = %#v", logResponse)
	}
	if _, err := control.RunLog(ctx, api.Actor{Identity: "agent@host"}, enqueued.ID, "test", "vet"); err == nil {
		t.Fatal("log view served a burn/step pair that was never recorded")
	}
	if _, err := control.Run(ctx, api.Actor{Identity: "agent@host"}, "run-does-not-exist"); err == nil {
		t.Fatal("run detail view served an unknown run")
	}
}

func TestDefaultBranchUsesOrdinaryCIPublicationAndSync(t *testing.T) {
	fixture := newControlFixture(t, JobResult{
		Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)},
	})
	const sha = "dddddddddddddddddddddddddddddddddddddddd"
	ctx := context.Background()
	enqueued, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "receive-main", Repository: fixture.repo, Branch: fixture.repo.DefaultBranch,
		SHA: sha, Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.scheduler.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	finished, err := fixture.store.Run(ctx, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != model.RunPassed || finished.Phase != "passed" || finished.Error != "" {
		t.Fatalf("default branch run = %#v", finished)
	}
	if len(fixture.git.syncedBranches) != 1 || fixture.git.syncedBranches[0] != "oberth:main:"+sha {
		t.Fatalf("default branch publication = %#v", fixture.git.syncedBranches)
	}
	_, err = fixture.api(t).CallTool(ctx, api.Actor{Identity: "agent@host"}, "sync", json.RawMessage(`{"sha":"`+sha+`"}`))
	if err != nil {
		t.Fatalf("default branch sync: %v", err)
	}
	if len(fixture.git.syncedBranches) != 2 || fixture.git.syncedBranches[1] != "oberth:main:"+sha {
		t.Fatalf("default branch sync = %#v", fixture.git.syncedBranches)
	}
}

func TestSchedulerCompletesSupersedeAndRestartCancellationObligations(t *testing.T) {
	fixture := newControlFixture(t)
	ctx := context.Background()
	first, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "receive-first", Repository: fixture.repo, Branch: "feature/cancel",
		SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
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
	if _, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "receive-second", Repository: fixture.repo, Branch: "feature/cancel",
		OldSHA: first.SHA, SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Actor: "agent@host",
	}); err != nil {
		t.Fatal(err)
	}
	if len(fixture.jobs.deleted) != 1 || fixture.jobs.deleted[0] != jobName {
		t.Fatalf("supersede deletions = %#v", fixture.jobs.deleted)
	}
	pending, err := fixture.store.PendingRunCancellations(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending supersede cancellations = %#v, %v", pending, err)
	}
	second, err := fixture.store.ClaimNextRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.FinishRun(ctx, second.ID, model.RunResult{Status: model.RunInterrupted}); err != nil {
		t.Fatal(err)
	}

	restart, err := fixture.store.EnqueueRun(ctx, model.RunSpec{
		RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: "feature/restart-intent",
		SHA: "cccccccccccccccccccccccccccccccccccccccc", Actor: "agent@host", Trigger: "branch",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	restartJob, _ := deterministicJobName(fixture.repo.Name, restart.ID)
	if _, err := fixture.store.SetRunJobName(ctx, restart.ID, restartJob); err != nil {
		t.Fatal(err)
	}
	// This models a crash after the durable Job intent and before idempotent
	// Create/observe. Owner recovery records the obligation even if no Job ever
	// appeared; Delete is required to be idempotent.
	if err := fixture.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, filepath.Join(fixture.root, "oberth.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	fixture.store = reopened
	t.Cleanup(func() { _ = reopened.Close() })
	recoveryScheduler, err := NewScheduler(SchedulerConfig{
		Store: reopened, Git: fixture.git, Logs: fixture.logs, Jobs: fixture.jobs, ReleaseJobs: fixture.jobs,
		Auditor: reopened, Signals: fixture.signals, WorkspaceRoot: filepath.Join(fixture.root, "work"), MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := recoveryScheduler.completePendingCancellations(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fixture.jobs.deleted) != 2 || fixture.jobs.deleted[1] != restartJob {
		t.Fatalf("restart deletions = %#v", fixture.jobs.deleted)
	}
}

func TestDeterministicJobNameExposesRepositoryAndDistinctRunIdentity(t *testing.T) {
	firstRun := "2d2f0986" + strings.Repeat("a", 24)
	secondRun := "2d2f0986" + strings.Repeat("b", 24)
	first, err := deterministicJobName("Acme_CLI", firstRun)
	if err != nil {
		t.Fatal(err)
	}
	second, err := deterministicJobName("Acme_CLI", secondRun)
	if err != nil {
		t.Fatal(err)
	}
	if prefix := "oberth-acme-cli-2d2f0986-"; !strings.HasPrefix(first, prefix) || !strings.HasPrefix(second, prefix) {
		t.Fatalf("Job names = %q and %q, want prefix %q", first, second, prefix)
	}
	if first == second || len(first) > 57 || len(second) > 57 {
		t.Fatalf("Job names = %q and %q, want distinct names no longer than 57 bytes", first, second)
	}
	long, err := deterministicJobName(strings.Repeat("repository_", 8), firstRun)
	if err != nil || len(long) > 57 {
		t.Fatalf("long repository Job name = %q, %v", long, err)
	}
	for _, repository := range []string{"", "***"} {
		if _, err := deterministicJobName(repository, firstRun); err == nil {
			t.Fatalf("repository %q formed a Job name", repository)
		}
	}
}

func TestSupersessionBetweenJobIntentAndCreateCannotOrphanJob(t *testing.T) {
	fixture := newControlFixture(t)
	jobs := &createCancellationRaceJobs{createStarted: make(chan struct{}), allowCreate: make(chan struct{})}
	scheduler, err := NewScheduler(SchedulerConfig{
		Store: fixture.store, Git: fixture.git, Logs: fixture.logs, Jobs: jobs, ReleaseJobs: jobs,
		Auditor: fixture.store, Signals: fixture.signals,
		WorkspaceRoot: filepath.Join(fixture.root, "create-cancel-race"), MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first, err := scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "receive-create-race-first", Repository: fixture.repo, Branch: "feature/create-race",
		SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	processed := make(chan error, 1)
	go func() { processed <- scheduler.ProcessNext(ctx) }()
	select {
	case <-jobs.createStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not reach Job creation")
	}
	if _, err := scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "receive-create-race-second", Repository: fixture.repo, Branch: "feature/create-race",
		OldSHA: first.SHA, SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Actor: "agent@host",
	}); err != nil {
		t.Fatal(err)
	}
	close(jobs.allowCreate)
	select {
	case err := <-processed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("superseded worker did not finish")
	}
	jobs.mu.Lock()
	created, before, after := jobs.created, jobs.deleteBefore, jobs.deleteAfter
	jobs.mu.Unlock()
	if created || before != 1 || after != 1 {
		t.Fatalf("Job create/cancel handshake = created:%v delete-before:%d delete-after:%d", created, before, after)
	}
	interrupted, err := fixture.store.Run(ctx, first.ID)
	if err != nil || interrupted.Status != model.RunInterrupted {
		t.Fatalf("superseded run = %#v, %v", interrupted, err)
	}
	pending, err := fixture.store.PendingRunCancellations(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending cancellations after direct delete = %#v, %v", pending, err)
	}
}

func TestSupersessionDeleteFailureRetainsCancellationUntilRetry(t *testing.T) {
	fixture := newControlFixture(t)
	jobs := &createCancellationRaceJobs{
		createStarted: make(chan struct{}), allowCreate: make(chan struct{}), deleteFailures: 2,
	}
	scheduler, err := NewScheduler(SchedulerConfig{
		Store: fixture.store, Git: fixture.git, Logs: fixture.logs, Jobs: jobs, ReleaseJobs: jobs,
		Auditor: fixture.store, Signals: fixture.signals,
		WorkspaceRoot: filepath.Join(fixture.root, "create-delete-failure"), MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first, err := scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "receive-delete-failure-first", Repository: fixture.repo, Branch: "feature/delete-failure",
		SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	processed := make(chan error, 1)
	go func() { processed <- scheduler.ProcessNext(ctx) }()
	select {
	case <-jobs.createStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not reach Job creation")
	}
	if _, err := scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "receive-delete-failure-second", Repository: fixture.repo, Branch: "feature/delete-failure",
		OldSHA: first.SHA, SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Actor: "agent@host",
	}); err == nil || !strings.Contains(err.Error(), "injected Job deletion failure") {
		t.Fatalf("superseding enqueue error = %v, want deletion failure", err)
	}
	close(jobs.allowCreate)
	select {
	case err := <-processed:
		if err == nil || !strings.Contains(err.Error(), "injected Job deletion failure") {
			t.Fatalf("worker error = %v, want deletion failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("superseded worker did not finish")
	}
	pending, err := fixture.store.PendingRunCancellations(ctx)
	if err != nil || len(pending) != 1 || pending[0].RunID != first.ID {
		t.Fatalf("pending cancellation after failed deletes = %#v, %v", pending, err)
	}
	if err := scheduler.completePendingCancellations(ctx); err != nil {
		t.Fatal(err)
	}
	pending, err = fixture.store.PendingRunCancellations(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending cancellation after retry = %#v, %v", pending, err)
	}
	jobs.mu.Lock()
	created, before, after := jobs.created, jobs.deleteBefore, jobs.deleteAfter
	jobs.mu.Unlock()
	if created || before != 1 || after != 2 {
		t.Fatalf("Job delete retries = created:%v before:%d after:%d", created, before, after)
	}
}

func TestPostCreateReadAndDeleteFailureRecoversExactJobOnRestart(t *testing.T) {
	fixture := newControlFixture(t)
	wrappedStore := &runReadFailStore{Store: fixture.store, failNextRun: true}
	jobs := &flakyDeleteJobs{fakeJobs: &fakeJobs{}, deleteFailures: 1}
	scheduler, err := NewScheduler(SchedulerConfig{
		Store: wrappedStore, Git: fixture.git, Logs: fixture.logs, Jobs: jobs, ReleaseJobs: jobs,
		Auditor: wrappedStore, Signals: fixture.signals,
		WorkspaceRoot: filepath.Join(fixture.root, "post-create-delete-failure"), MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	enqueued, err := scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "receive-post-create-read-failure", Repository: fixture.repo, Branch: "feature/read-failure",
		SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.ProcessNext(ctx); err == nil || !strings.Contains(err.Error(), "delete Job") {
		t.Fatalf("process error = %v, want exact Job deletion failure", err)
	}
	running, err := fixture.store.Run(ctx, enqueued.ID)
	if err != nil || running.Status != model.RunRunning || running.JobName == "" {
		t.Fatalf("run retained for owner recovery = %#v, %v", running, err)
	}
	jobName := running.JobName
	if err := fixture.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, filepath.Join(fixture.root, "oberth.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	recovery, err := NewScheduler(SchedulerConfig{
		Store: reopened, Git: fixture.git, Logs: fixture.logs, Jobs: jobs, ReleaseJobs: jobs,
		Auditor: reopened, Signals: fixture.signals,
		WorkspaceRoot: filepath.Join(fixture.root, "post-create-delete-failure"), MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.completePendingCancellations(ctx); err != nil {
		t.Fatal(err)
	}
	jobs.mu.Lock()
	attempted := append([]string(nil), jobs.attempted...)
	deleted := append([]string(nil), jobs.deleted...)
	jobs.mu.Unlock()
	if len(attempted) != 2 || attempted[0] != jobName || attempted[1] != jobName || len(deleted) != 1 || deleted[0] != jobName {
		t.Fatalf("exact Job recovery attempts = %#v, successful = %#v", attempted, deleted)
	}
}

func TestSchedulerShutdownCancelsAndJoinsWorkers(t *testing.T) {
	fixture := newControlFixture(t)
	jobs := newBlockingJobs()
	scheduler, err := NewScheduler(SchedulerConfig{
		Store: fixture.store, Git: fixture.git, Logs: fixture.logs, Jobs: jobs, ReleaseJobs: jobs,
		Auditor: fixture.store, Signals: fixture.signals,
		WorkspaceRoot: filepath.Join(fixture.root, "shutdown-work"), MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	enqueued, err := scheduler.EnqueueCI(context.Background(), CIRequest{
		EventID: "receive-shutdown", Repository: fixture.repo, Branch: "feature/shutdown",
		SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runReturned := make(chan error, 1)
	go func() { runReturned <- scheduler.Run(ctx) }()
	select {
	case <-jobs.started:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not start the Job")
	}
	jobName, err := deterministicJobName(fixture.repo.Name, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	running, err := fixture.store.Run(context.Background(), enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if running.JobName != jobName {
		t.Fatalf("persisted Job intent = %q, want %q", running.JobName, jobName)
	}

	cancel()
	select {
	case <-jobs.canceled:
	case <-time.After(time.Second):
		t.Fatal("worker did not observe scheduler cancellation")
	}
	select {
	case err := <-runReturned:
		t.Fatalf("scheduler returned before its worker exited: %v", err)
	default:
	}
	close(jobs.release)
	select {
	case err := <-runReturned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler did not join its canceled worker")
	}
	select {
	case deleted := <-jobs.deleted:
		if deleted != jobName {
			t.Fatalf("deleted Job = %q, want %q", deleted, jobName)
		}
	default:
		t.Fatal("scheduler did not delete the canceled Job")
	}
	finished, err := fixture.store.Run(context.Background(), enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != model.RunInterrupted {
		t.Fatalf("shutdown run status = %q", finished.Status)
	}
}

func TestJobObservationCancellationDoesNotImpersonateSchedulerShutdown(t *testing.T) {
	for name, waitErr := range map[string]error{
		"canceled": context.Canceled,
		"deadline": context.DeadlineExceeded,
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newControlFixture(t)
			fixture.jobs.waitErr = waitErr
			enqueued, err := fixture.scheduler.EnqueueCI(context.Background(), CIRequest{
				EventID: "receive-observation-" + name, Repository: fixture.repo, Branch: "feature/observation-" + name,
				SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actor: "agent@host",
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := fixture.scheduler.ProcessNext(context.Background()); err != nil {
				t.Fatal(err)
			}
			finished, err := fixture.store.Run(context.Background(), enqueued.ID)
			if err != nil {
				t.Fatal(err)
			}
			if finished.Status != model.RunFailed || finished.Phase != "job" || finished.Error != waitErr.Error() {
				t.Fatalf("inner observation cancellation = %#v, want failed job without scheduler shutdown", finished)
			}
			if len(fixture.jobs.deleted) != 0 {
				t.Fatalf("inner observation cancellation deleted Jobs as scheduler shutdown: %v", fixture.jobs.deleted)
			}
		})
	}
}

func TestReleaseChecksOutCommitButPublishesRawTagObject(t *testing.T) {
	fixture := newControlFixture(t, JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)}})
	const objectSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const commitSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	result, err := fixture.scheduler.AdmitRelease(context.Background(), ReleaseRequest{
		EventID: "receive-tag", Repository: fixture.repo, Tag: "v1.2.3",
		ObjectSHA: objectSHA, CommitSHA: commitSHA, Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.SHA != objectSHA || result.TestedSHA != commitSHA || !result.Release {
		t.Fatalf("release run = %#v", result.Run)
	}
	if err := fixture.scheduler.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fixture.git.checkouts) != 1 || fixture.git.checkouts[0] != commitSHA {
		t.Fatalf("release checkouts = %#v", fixture.git.checkouts)
	}
	if len(fixture.git.syncedTags) != 1 || fixture.git.syncedTags[0] != "oberth:v1.2.3:"+objectSHA {
		t.Fatalf("release tag syncs = %#v", fixture.git.syncedTags)
	}
	if len(fixture.jobs.createdReleases) != 1 || len(fixture.jobs.createdCI) != 0 {
		t.Fatalf("release/CI Jobs = %d/%d", len(fixture.jobs.createdReleases), len(fixture.jobs.createdCI))
	}
}

func TestBranchAndTagPublicationRecoverAfterPushBeforeFinalization(t *testing.T) {
	const (
		branchSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		tagObject = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		tagCommit = "cccccccccccccccccccccccccccccccccccccccc"
	)
	tests := []struct {
		name    string
		enqueue func(context.Context, *Scheduler, model.Repository) (model.Run, error)
		ref     string
		sha     string
	}{
		{
			name: "branch",
			enqueue: func(ctx context.Context, scheduler *Scheduler, repository model.Repository) (model.Run, error) {
				result, err := scheduler.EnqueueCI(ctx, CIRequest{
					EventID: "receive-branch-crash", Repository: repository,
					Branch: "feature/publication-crash", SHA: branchSHA, Actor: "agent@host",
				})
				return result.Run, err
			},
			ref: "refs/heads/feature/publication-crash", sha: branchSHA,
		},
		{
			name: "tag",
			enqueue: func(ctx context.Context, scheduler *Scheduler, repository model.Repository) (model.Run, error) {
				result, err := scheduler.AdmitRelease(ctx, ReleaseRequest{
					EventID: "receive-tag-crash", Repository: repository, Tag: "v1.2.3",
					ObjectSHA: tagObject, CommitSHA: tagCommit, Actor: "agent@host",
				})
				return result.Run, err
			},
			ref: "refs/tags/v1.2.3", sha: tagObject,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newControlFixture(t, JobResult{
				Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)},
			})
			failing := &finalizeFailStore{Store: fixture.store, failNext: true}
			scheduler, err := NewScheduler(SchedulerConfig{
				Store: failing, Git: fixture.git, Logs: fixture.logs, Jobs: fixture.jobs, ReleaseJobs: fixture.jobs,
				Auditor: fixture.store, Signals: fixture.signals,
				WorkspaceRoot: filepath.Join(fixture.root, "crash-work"), MaxConcurrent: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			run, err := test.enqueue(context.Background(), scheduler, fixture.repo)
			if err != nil {
				t.Fatal(err)
			}
			if err := scheduler.ProcessNext(context.Background()); err == nil || !strings.Contains(err.Error(), "injected crash") {
				t.Fatalf("ProcessNext error = %v, want injected crash", err)
			}
			sha, exists, err := fixture.git.RemoteRef(context.Background(), fixture.repo.Name, test.ref)
			if err != nil || !exists || sha != test.sha {
				t.Fatalf("remote after ambiguous completion = %q, %v, %v", sha, exists, err)
			}
			pending, err := fixture.store.PendingPublications(context.Background())
			if err != nil || len(pending) != 1 || pending[0].RunID != run.ID {
				t.Fatalf("pending publication = %#v, %v", pending, err)
			}
			stillRunning, err := fixture.store.Run(context.Background(), run.ID)
			if err != nil || stillRunning.Status != model.RunRunning || stillRunning.Phase != "publishing" {
				t.Fatalf("publication-owned run = %#v, %v", stillRunning, err)
			}

			beforeMutations := len(fixture.git.syncedBranches) + len(fixture.git.syncedTags)
			recovery, err := NewScheduler(SchedulerConfig{
				Store: fixture.store, Git: fixture.git, Logs: fixture.logs, Jobs: fixture.jobs, ReleaseJobs: fixture.jobs,
				Auditor: fixture.store, Signals: fixture.signals,
				WorkspaceRoot: filepath.Join(fixture.root, "recovery-work"), MaxConcurrent: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := recovery.recoverPendingPublications(context.Background()); err != nil {
				t.Fatal(err)
			}
			finished, err := fixture.store.Run(context.Background(), run.ID)
			if err != nil || finished.Status != model.RunPassed {
				t.Fatalf("recovered run = %#v, %v", finished, err)
			}
			afterMutations := len(fixture.git.syncedBranches) + len(fixture.git.syncedTags)
			if afterMutations != beforeMutations {
				t.Fatalf("recovery repeated already-applied mutation: before=%d after=%d", beforeMutations, afterMutations)
			}
		})
	}
}

func TestBranchPublicationRecoveryForcePublishesOverAnyRemoteFeatureState(t *testing.T) {
	const (
		previous = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		result   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		other    = "cccccccccccccccccccccccccccccccccccccccc"
	)
	for _, test := range []struct {
		name      string
		remoteSHA string
	}{
		{name: "exact previous", remoteSHA: previous},
		{name: "concurrently moved feature branch", remoteSHA: other},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newControlFixture(t)
			run, err := fixture.store.EnqueueRun(context.Background(), model.RunSpec{
				RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: "feature/recover",
				SHA: result, Actor: "agent@host", Trigger: "branch", TestedSHA: result,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.store.ClaimNextRun(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.store.BeginPublication(context.Background(), model.PublicationSpec{
				RepoID: fixture.repo.ID, RunID: run.ID, RefKind: model.RefBranch, Ref: run.Ref,
				PreviousSHA: previous, ResultSHA: result, Actor: run.Actor,
			}); err != nil {
				t.Fatal(err)
			}
			fixture.git.setRemoteRefLocked("refs/heads/feature/recover", test.remoteSHA)
			if err := fixture.scheduler.recoverPendingPublications(context.Background()); err != nil {
				t.Fatal(err)
			}
			finished, err := fixture.store.Run(context.Background(), run.ID)
			if err != nil || finished.Status != model.RunPassed {
				t.Fatalf("recovered run = %#v, %v", finished, err)
			}
			if len(fixture.git.syncedBranches) != 1 {
				t.Fatalf("publication pushes = %#v, want one", fixture.git.syncedBranches)
			}
			remote, exists, lookupErr := fixture.git.RemoteRef(context.Background(), fixture.repo.Name, "refs/heads/feature/recover")
			if lookupErr != nil || !exists || remote != result {
				t.Fatalf("force-published branch = %q, %v, %v", remote, exists, lookupErr)
			}
		})
	}
}

func TestDivergentPromotionRecoversPushBeforeAtomicFinalization(t *testing.T) {
	const (
		sourceSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		baseSHA   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		mergedSHA = "cccccccccccccccccccccccccccccccccccccccc"
	)
	fixture := newControlFixture(t, JobResult{
		Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)},
	})
	seedGreen := func() {
		run, err := fixture.store.EnqueueRun(context.Background(), model.RunSpec{
			RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: "feature/promote-crash",
			SHA: sourceSHA, Actor: "agent@host", Trigger: "branch", TestedSHA: sourceSHA,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.ClaimNextRun(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.FinishRun(context.Background(), run.ID, model.RunResult{Status: model.RunPassed, TestedSHA: sourceSHA}); err != nil {
			t.Fatal(err)
		}
	}
	seedGreen()
	fixture.git.plan = gitcache.MergeCandidate{BaseSHA: baseSHA, MergedSHA: mergedSHA}
	value, err := fixture.api(t).CallTool(context.Background(), api.Actor{Identity: "agent@host"}, "promote", json.RawMessage(`{"sha":"`+sourceSHA+`","branch":"main"}`))
	if err != nil {
		t.Fatal(err)
	}
	pendingPromotion := requireToolPromotion(t, fixture, value)
	failing := &finalizeFailStore{Store: fixture.store, failNext: true}
	scheduler, err := NewScheduler(SchedulerConfig{
		Store: failing, Git: fixture.git, Logs: fixture.logs, Jobs: fixture.jobs, ReleaseJobs: fixture.jobs,
		Auditor: fixture.store, Signals: fixture.signals,
		WorkspaceRoot: filepath.Join(fixture.root, "promotion-crash-work"), MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.ProcessNext(context.Background()); err == nil || !strings.Contains(err.Error(), "injected crash") {
		t.Fatalf("ProcessNext error = %v, want injected crash", err)
	}
	stillPending, err := fixture.store.Promotion(context.Background(), pendingPromotion.ID)
	if err != nil || stillPending.Status != model.PromotionPending {
		t.Fatalf("promotion after crash = %#v, %v", stillPending, err)
	}
	if err := fixture.scheduler.recoverPendingPublications(context.Background()); err != nil {
		t.Fatal(err)
	}
	finished, err := fixture.store.Promotion(context.Background(), pendingPromotion.ID)
	if err != nil || finished.Status != model.PromotionPassed {
		t.Fatalf("recovered promotion = %#v, %v", finished, err)
	}
	if len(fixture.git.promotions) != 1 {
		t.Fatalf("promotion push repeated: %#v", fixture.git.promotions)
	}
}

func TestPublicationFailureTerminalizesWithoutBackgroundRetry(t *testing.T) {
	const (
		sourceSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		baseSHA   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	fixture := newControlFixture(t)
	green, err := fixture.store.EnqueueRun(context.Background(), model.RunSpec{
		RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: "feature/retry-publication",
		SHA: sourceSHA, Actor: "agent@host", Trigger: "branch", TestedSHA: sourceSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.ClaimNextRun(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.FinishRun(context.Background(), green.ID, model.RunResult{
		Status: model.RunPassed, TestedSHA: sourceSHA,
	}); err != nil {
		t.Fatal(err)
	}
	fixture.git.plan = gitcache.MergeCandidate{BaseSHA: baseSHA, MergedSHA: sourceSHA, FastForward: true}
	fixture.git.pushErr = errors.New("upstream unavailable")

	value, err := fixture.api(t).CallTool(context.Background(), api.Actor{Identity: "agent@host"}, "promote",
		json.RawMessage(`{"sha":"`+sourceSHA+`","branch":"main"}`))
	if err != nil {
		t.Fatal(err)
	}
	promotion := requireToolPromotion(t, fixture, value)
	if promotion.Status != model.PromotionFailed || !strings.Contains(promotion.Error, "upstream unavailable") {
		t.Fatalf("publication failure was not terminal: %#v", promotion)
	}
	pending, err := fixture.store.PendingPublications(context.Background())
	if err != nil || len(pending) != 0 {
		t.Fatalf("terminal publication left recovery work: %#v, %v", pending, err)
	}
	if len(fixture.git.promotions) != 1 {
		t.Fatalf("publication attempts = %#v, want one bounded attempt", fixture.git.promotions)
	}
}

func TestPromotionFastForwardDivergentAndNonFastForwardFailure(t *testing.T) {
	const sourceSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const baseSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const mergedSHA = "cccccccccccccccccccccccccccccccccccccccc"
	seedGreen := func(t *testing.T, fixture *controlFixture) {
		t.Helper()
		run, err := fixture.store.EnqueueRun(context.Background(), model.RunSpec{
			RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: "feature/promote",
			SHA: sourceSHA, Actor: "agent@host", Trigger: "branch", TestedSHA: sourceSHA,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.ClaimNextRun(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.FinishRun(context.Background(), run.ID, model.RunResult{Status: model.RunPassed, TestedSHA: sourceSHA}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("fast-forward reuses green tree", func(t *testing.T) {
		fixture := newControlFixture(t)
		seedGreen(t, fixture)
		fixture.git.plan = gitcache.MergeCandidate{BaseSHA: baseSHA, MergedSHA: sourceSHA, FastForward: true}
		value, err := fixture.api(t).CallTool(context.Background(), api.Actor{Identity: "agent@host"}, "promote", json.RawMessage(`{"sha":"`+sourceSHA+`","branch":"main"}`))
		if err != nil {
			t.Fatal(err)
		}
		promotion := requireToolPromotion(t, fixture, value)
		if promotion.Status != model.PromotionPassed || promotion.RunID != "" || len(fixture.git.promotions) != 1 {
			t.Fatalf("fast-forward promotion = %#v pushes=%#v", promotion, fixture.git.promotions)
		}
	})

	t.Run("already-contained source queues target tree CI", func(t *testing.T) {
		fixture := newControlFixture(t)
		seedGreen(t, fixture)
		fixture.git.plan = gitcache.MergeCandidate{BaseSHA: baseSHA, MergedSHA: baseSHA, FastForward: false}
		value, err := fixture.api(t).CallTool(context.Background(), api.Actor{Identity: "agent@host"}, "promote", json.RawMessage(`{"sha":"`+sourceSHA+`","branch":"main"}`))
		if err != nil {
			t.Fatal(err)
		}
		promotion := requireToolPromotion(t, fixture, value)
		if promotion.ID == "" || promotion.Status != model.PromotionPending || promotion.ResultSHA != baseSHA || promotion.RunID == "" || len(fixture.git.promotions) != 0 {
			t.Fatalf("already-contained promotion = %#v pushes=%#v", promotion, fixture.git.promotions)
		}
		run, err := fixture.store.Run(context.Background(), promotion.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != model.RunQueued || run.Trigger != "promotion" || run.SHA != baseSHA || run.TestedSHA != baseSHA || run.BaseSHA != baseSHA {
			t.Fatalf("already-contained promotion run = %#v", run)
		}
	})

	t.Run("fast-forward push recovers before finalization", func(t *testing.T) {
		fixture := newControlFixture(t)
		seedGreen(t, fixture)
		fixture.git.plan = gitcache.MergeCandidate{BaseSHA: baseSHA, MergedSHA: sourceSHA, FastForward: true}
		failing := &finalizeFailStore{Store: fixture.store, failNext: true}
		failingScheduler, err := NewScheduler(SchedulerConfig{
			Store: failing, Git: fixture.git, Logs: fixture.logs, Jobs: fixture.jobs, ReleaseJobs: fixture.jobs,
			Auditor: fixture.store, Signals: fixture.signals,
			WorkspaceRoot: filepath.Join(fixture.root, "promotion-fast-crash-work"), MaxConcurrent: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		service, err := NewAPI(APIConfig{
			Runs: fixture.store, History: fixture.store, Repositories: fixture.store,
			Issues: fixture.store, Promotions: fixture.store, PromotionRuns: fixture.store,
			Enqueues: failingScheduler, Git: fixture.git, Logs: fixture.logs, Auditor: fixture.store,
			Signals: fixture.signals, MaximumWait: 50 * time.Millisecond,
			PromotionWorkspaceRoot: filepath.Join(fixture.root, "promotion-fast-crash"),
		})
		if err != nil {
			t.Fatal(err)
		}
		value, err := service.CallTool(context.Background(), api.Actor{Identity: "agent@host"}, "promote", json.RawMessage(`{"sha":"`+sourceSHA+`","branch":"main"}`))
		if err != nil {
			t.Fatal(err)
		}
		returned := requireToolPromotion(t, fixture, value)
		if returned.ID == "" || returned.Status != model.PromotionPending {
			t.Fatalf("fast-forward recovery handle = %#v", returned)
		}
		pending, err := fixture.store.PendingPublications(context.Background())
		if err != nil || len(pending) != 1 || pending[0].PromotionID == "" || pending[0].RunID != "" {
			t.Fatalf("fast-forward pending publication = %#v, %v", pending, err)
		}
		promotionID := pending[0].PromotionID
		if returned.ID != promotionID {
			t.Fatalf("returned promotion ID %s, pending publication owns %s", returned.ID, promotionID)
		}
		before, err := fixture.store.Promotion(context.Background(), promotionID)
		if err != nil || before.Status != model.PromotionPending {
			t.Fatalf("fast-forward promotion before recovery = %#v, %v", before, err)
		}
		if err := failingScheduler.recoverPendingPublications(context.Background()); err != nil {
			t.Fatal(err)
		}
		after, err := fixture.store.Promotion(context.Background(), promotionID)
		if err != nil || after.Status != model.PromotionPassed || len(fixture.git.promotions) != 1 {
			t.Fatalf("fast-forward promotion after recovery = %#v pushes=%#v err=%v", after, fixture.git.promotions, err)
		}
	})

	t.Run("divergent tree gets a CI run", func(t *testing.T) {
		fixture := newControlFixture(t, JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)}})
		seedGreen(t, fixture)
		fixture.git.plan = gitcache.MergeCandidate{BaseSHA: baseSHA, MergedSHA: mergedSHA}
		value, err := fixture.api(t).CallTool(context.Background(), api.Actor{Identity: "agent@host"}, "promote", json.RawMessage(`{"sha":"`+sourceSHA+`","branch":"main"}`))
		if err != nil {
			t.Fatal(err)
		}
		pending := requireToolPromotion(t, fixture, value)
		if pending.Status != model.PromotionPending || pending.RunID == "" || len(fixture.git.promotions) != 0 {
			t.Fatalf("pending divergent promotion = %#v", pending)
		}
		if err := fixture.scheduler.ProcessNext(context.Background()); err != nil {
			t.Fatal(err)
		}
		finished, err := fixture.store.Promotion(context.Background(), pending.ID)
		if err != nil || finished.Status != model.PromotionPassed || len(fixture.git.promotions) != 1 {
			t.Fatalf("finished divergent promotion = %#v pushes=%#v err=%v", finished, fixture.git.promotions, err)
		}
	})

	t.Run("moved target is terminal failure", func(t *testing.T) {
		fixture := newControlFixture(t)
		seedGreen(t, fixture)
		fixture.git.plan = gitcache.MergeCandidate{BaseSHA: baseSHA, MergedSHA: sourceSHA, FastForward: true}
		fixture.git.pushErr = errors.New("non-fast-forward")
		fixture.git.pushMoveSHA = mergedSHA
		value, err := fixture.api(t).CallTool(context.Background(), api.Actor{Identity: "agent@host"}, "promote", json.RawMessage(`{"sha":"`+sourceSHA+`","branch":"main"}`))
		if err != nil {
			t.Fatal(err)
		}
		promotion := requireToolPromotion(t, fixture, value)
		if promotion.Status != model.PromotionFailed || promotion.Error == "" {
			t.Fatalf("non-fast-forward promotion = %#v", promotion)
		}
	})

	t.Run("infrastructure failure is terminal", func(t *testing.T) {
		fixture := newControlFixture(t)
		seedGreen(t, fixture)
		fixture.git.plan = gitcache.MergeCandidate{BaseSHA: baseSHA, MergedSHA: mergedSHA}
		value, err := fixture.api(t).CallTool(context.Background(), api.Actor{Identity: "agent@host"}, "promote", json.RawMessage(`{"sha":"`+sourceSHA+`","branch":"main"}`))
		if err != nil {
			t.Fatal(err)
		}
		pending := requireToolPromotion(t, fixture, value)
		fixture.git.checkoutErr = errors.New("workspace unavailable")
		if err := fixture.scheduler.ProcessNext(context.Background()); err != nil {
			t.Fatal(err)
		}
		failed, err := fixture.store.Promotion(context.Background(), pending.ID)
		if err != nil {
			t.Fatal(err)
		}
		if failed.Status != model.PromotionFailed || failed.Error == "" {
			t.Fatalf("infrastructure-failed promotion = %#v", failed)
		}
	})
}

func TestFastForwardPromotionPublicationWakesSameRefQueue(t *testing.T) {
	const (
		target    = "stage"
		sourceSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		baseSHA   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		nextSHA   = "cccccccccccccccccccccccccccccccccccccccc"
	)
	fixture := newControlFixture(t,
		JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)}},
	)
	seed, err := fixture.store.EnqueueRun(context.Background(), model.RunSpec{
		RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: "feature/promote-wake",
		SHA: sourceSHA, Actor: "agent@host", Trigger: "branch", TestedSHA: sourceSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.ClaimNextRun(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.FinishRun(context.Background(), seed.ID, model.RunResult{Status: model.RunPassed, TestedSHA: sourceSHA}); err != nil {
		t.Fatal(err)
	}
	fixture.git.plan = gitcache.MergeCandidate{BaseSHA: baseSHA, MergedSHA: sourceSHA, FastForward: true}
	fixture.git.promotionPublishStarted = make(chan struct{})
	fixture.git.promotionPublishRelease = make(chan struct{})
	var releaseOnce sync.Once
	releasePublication := func() { releaseOnce.Do(func() { close(fixture.git.promotionPublishRelease) }) }
	observed := &claimObservingStore{
		Store: fixture.store, empty: make(chan struct{}, 8), claimed: make(chan model.Run, 8),
	}
	scheduler, err := NewScheduler(SchedulerConfig{
		Store: observed, Git: fixture.git, Logs: fixture.logs, Jobs: fixture.jobs, ReleaseJobs: fixture.jobs,
		Auditor: fixture.store, Signals: fixture.signals,
		WorkspaceRoot: filepath.Join(fixture.root, "promotion-publication-wake-work"), MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	controlAPI, err := NewAPI(APIConfig{
		Runs: fixture.store, History: fixture.store, Repositories: fixture.store,
		Issues: fixture.store, Promotions: fixture.store, PromotionRuns: fixture.store,
		Enqueues: scheduler, Git: fixture.git, Logs: fixture.logs, Auditor: fixture.store,
		Signals: fixture.signals, MaximumWait: 50 * time.Millisecond,
		PromotionWorkspaceRoot: filepath.Join(fixture.root, "promotion-publication-wake-api"),
	})
	if err != nil {
		t.Fatal(err)
	}

	schedulerCtx, cancelScheduler := context.WithCancel(context.Background())
	var schedulerErr error
	schedulerDone := make(chan struct{})
	go func() {
		schedulerErr = scheduler.Run(schedulerCtx)
		close(schedulerDone)
	}()
	t.Cleanup(func() {
		releasePublication()
		cancelScheduler()
		select {
		case <-schedulerDone:
			if schedulerErr != nil {
				t.Errorf("scheduler run: %v", schedulerErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("scheduler did not stop after cancellation")
		}
	})
	// Run performs one initial empty claim, then consumes its self-wake and
	// performs a second. Waiting for both proves it is idle in select with no
	// residual wake before the publication race begins.
	for index := 0; index < 2; index++ {
		select {
		case <-observed.empty:
		case <-time.After(5 * time.Second):
			t.Fatalf("scheduler startup empty claim %d did not occur", index+1)
		}
	}

	type promotionResult struct {
		value any
		err   error
	}
	promotionDone := make(chan promotionResult, 1)
	go func() {
		value, callErr := controlAPI.CallTool(context.Background(), api.Actor{Identity: "agent@host"}, "promote",
			json.RawMessage(`{"sha":"`+sourceSHA+`","branch":"`+target+`"}`))
		promotionDone <- promotionResult{value: value, err: callErr}
	}()
	select {
	case <-fixture.git.promotionPublishStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("fast-forward promotion did not reach durable publication")
	}

	blocked, err := scheduler.EnqueueCI(context.Background(), CIRequest{
		EventID: "receive-promotion-publication-same-ref", Repository: fixture.repo,
		Branch: target, OldSHA: baseSHA, SHA: nextSHA, Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-observed.empty:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not consume the gated run's enqueue wake")
	}
	queued, err := fixture.store.Run(context.Background(), blocked.ID)
	if err != nil || queued.Status != model.RunQueued {
		t.Fatalf("same-ref run during fast-forward publication = %#v, %v", queued, err)
	}
	pending, err := fixture.store.PendingPublications(context.Background())
	if err != nil || len(pending) != 1 || pending[0].Ref != target || pending[0].RunID != "" {
		t.Fatalf("API-owned pending publication = %#v, %v", pending, err)
	}

	releasePublication()
	var promoted promotionResult
	select {
	case promoted = <-promotionDone:
	case <-time.After(5 * time.Second):
		t.Fatal("fast-forward promotion did not finish after publication release")
	}
	if promoted.err != nil {
		t.Fatal(promoted.err)
	}
	promotion := requireToolPromotion(t, fixture, promoted.value)
	if promotion.Status != model.PromotionPassed {
		t.Fatalf("fast-forward promotion = %#v", promotion)
	}
	select {
	case claimed := <-observed.claimed:
		if claimed.ID != blocked.ID {
			t.Fatalf("claim after fast-forward publication = %s, want %s", claimed.ID, blocked.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fast-forward publication finalization did not wake the gated queue")
	}
	waitForFixtureRunStatus(t, fixture, blocked.ID, model.RunPassed)
	remoteSHA, exists, err := fixture.git.RemoteRef(context.Background(), fixture.repo.Name, "refs/heads/"+target)
	if err != nil || !exists || remoteSHA != nextSHA {
		t.Fatalf("same-ref run after promotion publication = %q, %v, %v", remoteSHA, exists, err)
	}
}

func TestAdmittedPromotionAlwaysReturnsDurableID(t *testing.T) {
	const sourceSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const baseSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const mergedSHA = "cccccccccccccccccccccccccccccccccccccccc"
	seedGreen := func(t *testing.T, fixture *controlFixture, branch string) {
		t.Helper()
		run, err := fixture.store.EnqueueRun(context.Background(), model.RunSpec{
			RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: branch,
			SHA: sourceSHA, Actor: "agent@host", Trigger: "branch", TestedSHA: sourceSHA,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.ClaimNextRun(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.FinishRun(context.Background(), run.ID, model.RunResult{Status: model.RunPassed}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("merge planning failure", func(t *testing.T) {
		fixture := newControlFixture(t)
		seedGreen(t, fixture, "feature/conflict")
		fixture.git.prepareErr = errors.New("merge conflict")
		value, err := fixture.api(t).CallTool(context.Background(), api.Actor{Identity: "agent@host"}, "promote",
			json.RawMessage(`{"sha":"`+sourceSHA+`","branch":"main"}`))
		if err != nil {
			t.Fatal(err)
		}
		promotion := requireToolPromotion(t, fixture, value)
		if promotion.ID == "" || promotion.Status != model.PromotionFailed || !strings.Contains(promotion.Error, "merge conflict") {
			t.Fatalf("planning-failed promotion = %#v", promotion)
		}
		issue, issueErr := fixture.store.OpenCIIssue(context.Background(), fixture.repo.ID, "feature/conflict")
		if issueErr != nil || issue.CIOrigin != "promotion" || !strings.Contains(issue.Body, promotion.ID) {
			t.Fatalf("planning failure issue = %#v, %v", issue, issueErr)
		}
	})

	t.Run("pre-enqueue audit failure", func(t *testing.T) {
		fixture := newControlFixture(t)
		seedGreen(t, fixture, "feature/audit")
		fixture.git.plan = gitcache.MergeCandidate{BaseSHA: baseSHA, MergedSHA: mergedSHA}
		auditErr := errors.New("audit unavailable")
		service, err := NewAPI(APIConfig{
			Runs: fixture.store, History: fixture.store, Repositories: fixture.store,
			Issues: fixture.store, Promotions: fixture.store, PromotionRuns: fixture.store,
			Enqueues: fixture.scheduler, Git: fixture.git, Logs: fixture.logs,
			Auditor: &promotionAuditFailer{Store: fixture.store, err: auditErr},
			Signals: fixture.signals, MaximumWait: 50 * time.Millisecond,
			PromotionWorkspaceRoot: filepath.Join(fixture.root, "promotion-audit-failure"),
		})
		if err != nil {
			t.Fatal(err)
		}
		value, err := service.CallTool(context.Background(), api.Actor{Identity: "agent@host"}, "promote",
			json.RawMessage(`{"sha":"`+sourceSHA+`","branch":"main"}`))
		if err != nil {
			t.Fatal(err)
		}
		promotion := requireToolPromotion(t, fixture, value)
		if promotion.ID == "" || promotion.RunID != "" || promotion.Status != model.PromotionFailed || !strings.Contains(promotion.Error, auditErr.Error()) {
			t.Fatalf("audit-failed promotion = %#v", promotion)
		}
		if _, claimErr := fixture.store.ClaimNextRun(context.Background()); !queueEmpty(claimErr) {
			t.Fatalf("audit failure left claimable run: %v", claimErr)
		}
	})

	t.Run("blocked audit is a scheduler admission barrier", func(t *testing.T) {
		fixture := newControlFixture(t, JobResult{
			Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)},
		})
		seedGreen(t, fixture, "feature/audit-barrier")
		fixture.git.plan = gitcache.MergeCandidate{BaseSHA: baseSHA, MergedSHA: mergedSHA}
		auditErr := errors.New("audit unavailable")
		auditor := &blockingPromotionAuditor{
			Store: fixture.store, started: make(chan struct{}), release: make(chan struct{}), err: auditErr,
		}
		service, err := NewAPI(APIConfig{
			Runs: fixture.store, History: fixture.store, Repositories: fixture.store,
			Issues: fixture.store, Promotions: fixture.store, PromotionRuns: fixture.store,
			Enqueues: fixture.scheduler, Git: fixture.git, Logs: fixture.logs, Auditor: auditor,
			Signals: fixture.signals, MaximumWait: 50 * time.Millisecond,
			PromotionWorkspaceRoot: filepath.Join(fixture.root, "promotion-audit-barrier"),
		})
		if err != nil {
			t.Fatal(err)
		}
		type callResult struct {
			value any
			err   error
		}
		called := make(chan callResult, 1)
		go func() {
			value, callErr := service.CallTool(context.Background(), api.Actor{Identity: "agent@host"}, "promote",
				json.RawMessage(`{"sha":"`+sourceSHA+`","branch":"main"}`))
			called <- callResult{value: value, err: callErr}
		}()
		var releaseAudit sync.Once
		t.Cleanup(func() { releaseAudit.Do(func() { close(auditor.release) }) })
		select {
		case <-auditor.started:
		case <-time.After(5 * time.Second):
			t.Fatal("promotion audit did not reach barrier")
		}

		const competingSHA = "dddddddddddddddddddddddddddddddddddddddd"
		competing, err := fixture.scheduler.EnqueueCI(context.Background(), CIRequest{
			EventID: "promotion-audit-competing-wake", Repository: fixture.repo,
			Branch: "feature/competing", SHA: competingSHA, Actor: "agent@host",
		})
		if err != nil {
			t.Fatal(err)
		}
		competingDone := fixture.signals.Run(competing.ID)
		schedulerCtx, cancelScheduler := context.WithCancel(context.Background())
		t.Cleanup(cancelScheduler)
		schedulerDone := make(chan error, 1)
		go func() { schedulerDone <- fixture.scheduler.Run(schedulerCtx) }()
		select {
		case <-competingDone:
		case <-time.After(5 * time.Second):
			t.Fatal("active scheduler did not process competing wake")
		}
		finished, err := fixture.store.Run(context.Background(), competing.ID)
		if err != nil || finished.Status != model.RunPassed {
			t.Fatalf("competing run = %#v, %v", finished, err)
		}
		recent, err := fixture.store.ListRecentRuns(context.Background(), model.RunListFilter{RepoID: fixture.repo.ID, Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range recent {
			if run.Trigger == "promotion" {
				t.Fatalf("promotion run became visible while audit was blocked: %#v", run)
			}
		}
		fixture.git.mu.Lock()
		pushes := append([]string(nil), fixture.git.promotions...)
		fixture.git.mu.Unlock()
		if len(pushes) != 0 {
			t.Fatalf("promotion published while audit was blocked: %#v", pushes)
		}

		releaseAudit.Do(func() { close(auditor.release) })
		var result callResult
		select {
		case result = <-called:
		case <-time.After(5 * time.Second):
			t.Fatal("promotion call did not finish after audit release")
		}
		if result.err != nil {
			t.Fatal(result.err)
		}
		promotion := requireToolPromotion(t, fixture, result.value)
		if promotion.ID == "" || promotion.RunID != "" || promotion.Status != model.PromotionFailed || !strings.Contains(promotion.Error, auditErr.Error()) {
			t.Fatalf("barrier-failed promotion = %#v", promotion)
		}
		if _, claimErr := fixture.store.ClaimNextRun(context.Background()); !queueEmpty(claimErr) {
			t.Fatalf("audit barrier left claimable promotion work: %v", claimErr)
		}
		cancelScheduler()
		select {
		case runErr := <-schedulerDone:
			if runErr != nil {
				t.Fatal(runErr)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("scheduler did not stop")
		}
	})
}

func TestAdmittedPromotionCompensationSurvivesRequestCancellation(t *testing.T) {
	const sourceSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const baseSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const mergedSHA = "cccccccccccccccccccccccccccccccccccccccc"
	seedGreen := func(t *testing.T, fixture *controlFixture, branch string) {
		t.Helper()
		enqueued, err := fixture.store.EnqueueRun(context.Background(), model.RunSpec{
			RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: branch,
			SHA: sourceSHA, Actor: "agent@host", Trigger: "branch", TestedSHA: sourceSHA,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.ClaimNextRun(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.FinishRun(context.Background(), enqueued.ID, model.RunResult{Status: model.RunPassed}); err != nil {
			t.Fatal(err)
		}
	}
	type callResult struct {
		value any
		err   error
	}
	assertTerminal := func(t *testing.T, fixture *controlFixture, promotion model.Promotion, branch, workspaceRoot string, wantRun bool) {
		t.Helper()
		if promotion.ID == "" || promotion.Status != model.PromotionFailed || (promotion.RunID != "") != wantRun || !strings.Contains(promotion.Error, context.Canceled.Error()) {
			t.Fatalf("cancellation-compensated promotion = %#v", promotion)
		}
		durable, err := fixture.store.Promotion(context.Background(), promotion.ID)
		if err != nil || durable.Status != model.PromotionFailed || durable.RunID != promotion.RunID {
			t.Fatalf("durable cancellation result = %#v, %v", durable, err)
		}
		issue, err := fixture.store.OpenCIIssue(context.Background(), fixture.repo.ID, branch)
		if err != nil || issue.CIOrigin != "promotion" || !strings.Contains(issue.Body, promotion.ID) {
			t.Fatalf("cancellation issue projection = %#v, %v", issue, err)
		}
		if wantRun {
			run, runErr := fixture.store.Run(context.Background(), promotion.RunID)
			if runErr != nil || run.Status != model.RunFailed {
				t.Fatalf("compensated promotion run = %#v, %v", run, runErr)
			}
		}
		if _, claimErr := fixture.store.ClaimNextRun(context.Background()); !queueEmpty(claimErr) {
			t.Fatalf("cancellation left claimable promotion work: %v", claimErr)
		}
		assertWorkspaceMissing(t, mustPromotionWorkspacePath(t, workspaceRoot, promotion.ID))
	}

	t.Run("planning", func(t *testing.T) {
		fixture := newControlFixture(t)
		branch := "feature/cancel-planning"
		seedGreen(t, fixture, branch)
		fixture.git.prepareStarted = make(chan struct{})
		fixture.git.prepareCancel = true
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		service := fixture.api(t)
		called := make(chan callResult, 1)
		go func() {
			value, err := service.CallTool(ctx, api.Actor{Identity: "agent@host"}, "promote",
				json.RawMessage(`{"sha":"`+sourceSHA+`","branch":"main"}`))
			called <- callResult{value: value, err: err}
		}()
		select {
		case <-fixture.git.prepareStarted:
		case <-time.After(5 * time.Second):
			t.Fatal("promotion planning did not start")
		}
		cancel()
		select {
		case result := <-called:
			if result.err != nil {
				t.Fatal(result.err)
			}
			assertTerminal(t, fixture, requireToolPromotion(t, fixture, result.value), branch, filepath.Join(fixture.root, "promotion-work"), false)
		case <-time.After(5 * time.Second):
			t.Fatal("planning cancellation did not compensate")
		}
	})

	t.Run("audit", func(t *testing.T) {
		fixture := newControlFixture(t)
		branch := "feature/cancel-audit"
		seedGreen(t, fixture, branch)
		fixture.git.plan = gitcache.MergeCandidate{BaseSHA: baseSHA, MergedSHA: mergedSHA}
		auditor := &blockingPromotionAuditor{Store: fixture.store, started: make(chan struct{}), waitCancel: true}
		service, err := NewAPI(APIConfig{
			Runs: fixture.store, History: fixture.store, Repositories: fixture.store,
			Issues: fixture.store, Promotions: fixture.store, PromotionRuns: fixture.store,
			Enqueues: fixture.scheduler, Git: fixture.git, Logs: fixture.logs, Auditor: auditor,
			Signals: fixture.signals, PromotionWorkspaceRoot: filepath.Join(fixture.root, "cancel-audit"),
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		called := make(chan callResult, 1)
		go func() {
			value, callErr := service.CallTool(ctx, api.Actor{Identity: "agent@host"}, "promote",
				json.RawMessage(`{"sha":"`+sourceSHA+`","branch":"main"}`))
			called <- callResult{value: value, err: callErr}
		}()
		select {
		case <-auditor.started:
		case <-time.After(5 * time.Second):
			t.Fatal("promotion audit did not start")
		}
		cancel()
		select {
		case result := <-called:
			if result.err != nil {
				t.Fatal(result.err)
			}
			assertTerminal(t, fixture, requireToolPromotion(t, fixture, result.value), branch, filepath.Join(fixture.root, "cancel-audit"), false)
		case <-time.After(5 * time.Second):
			t.Fatal("audit cancellation did not compensate")
		}
	})

	t.Run("enqueue notification", func(t *testing.T) {
		fixture := newControlFixture(t)
		branch := "feature/cancel-enqueue"
		seedGreen(t, fixture, branch)
		fixture.git.plan = gitcache.MergeCandidate{BaseSHA: baseSHA, MergedSHA: mergedSHA}
		observer := &cancelingEnqueueObserver{started: make(chan struct{})}
		service, err := NewAPI(APIConfig{
			Runs: fixture.store, History: fixture.store, Repositories: fixture.store,
			Issues: fixture.store, Promotions: fixture.store, PromotionRuns: fixture.store,
			Enqueues: observer, Git: fixture.git, Logs: fixture.logs, Auditor: fixture.store,
			Signals: fixture.signals, PromotionWorkspaceRoot: filepath.Join(fixture.root, "cancel-enqueue"),
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		called := make(chan callResult, 1)
		go func() {
			value, callErr := service.CallTool(ctx, api.Actor{Identity: "agent@host"}, "promote",
				json.RawMessage(`{"sha":"`+sourceSHA+`","branch":"main"}`))
			called <- callResult{value: value, err: callErr}
		}()
		select {
		case <-observer.started:
		case <-time.After(5 * time.Second):
			t.Fatal("promotion enqueue notification did not start")
		}
		cancel()
		select {
		case result := <-called:
			if result.err != nil {
				t.Fatal(result.err)
			}
			assertTerminal(t, fixture, requireToolPromotion(t, fixture, result.value), branch, filepath.Join(fixture.root, "cancel-enqueue"), true)
		case <-time.After(5 * time.Second):
			t.Fatal("enqueue cancellation did not compensate")
		}
	})
}

func TestStatusUsesDirectIssueLookupAndPropagatesRenewErrors(t *testing.T) {
	fixture := newControlFixture(t, JobResult{
		Status: model.RunFailed, Phase: "test", FailedBurn: "test", FailedStep: "unit",
		Error: "red", Steps: []model.StepResult{stepResult(model.StepFailed)},
	})
	ctx := context.Background()
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "status-direct-issue", Repository: fixture.repo, Branch: "feature/direct",
		SHA: sha, Actor: "agent@host",
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.scheduler.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}

	listErr := errors.New("bounded issue list must not be used")
	service, err := NewAPI(APIConfig{
		Runs: fixture.store, History: fixture.store, Repositories: fixture.store,
		Issues: &issueListFailer{Store: fixture.store, err: listErr}, Promotions: fixture.store,
		PromotionRuns: fixture.store, Enqueues: fixture.scheduler, Git: fixture.git, Logs: fixture.logs,
		Auditor: fixture.store, Signals: fixture.signals,
		PromotionWorkspaceRoot: filepath.Join(fixture.root, "status-direct"),
	})
	if err != nil {
		t.Fatal(err)
	}
	value, err := service.CallTool(ctx, api.Actor{Identity: "agent@host"}, "status",
		json.RawMessage(`{"ref":"feature/direct"}`))
	if err != nil {
		t.Fatal(err)
	}
	if status := value.(StatusResponse); status.Issue == nil {
		t.Fatalf("direct status issue = %#v", status)
	}
	renewErr := errors.New("sqlite unavailable")
	service, err = NewAPI(APIConfig{
		Runs: fixture.store, History: fixture.store, Repositories: fixture.store,
		Issues: &issueRenewFailer{Store: fixture.store, err: renewErr}, Promotions: fixture.store,
		PromotionRuns: fixture.store, Enqueues: fixture.scheduler, Git: fixture.git, Logs: fixture.logs,
		Auditor: fixture.store, Signals: fixture.signals,
		PromotionWorkspaceRoot: filepath.Join(fixture.root, "status-renew"),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.CallTool(ctx, api.Actor{Identity: "agent@host"}, "status",
		json.RawMessage(`{"ref":"feature/direct"}`))
	if !errors.Is(err, renewErr) {
		t.Fatalf("status renew error = %v, want %v", err, renewErr)
	}

	issue, err := fixture.store.CreateManualIssue(ctx, "agent@host", model.ManualIssueSpec{Title: "manual", Body: "body"})
	if err != nil {
		t.Fatal(err)
	}
	service, err = NewAPI(APIConfig{
		Runs: fixture.store, History: fixture.store, Repositories: fixture.store,
		Issues: &issueRenewFailer{Store: fixture.store, err: renewErr}, Promotions: fixture.store,
		PromotionRuns: fixture.store, Enqueues: fixture.scheduler, Git: fixture.git, Logs: fixture.logs,
		Auditor: fixture.store, Signals: fixture.signals,
		PromotionWorkspaceRoot: filepath.Join(fixture.root, "issue-renew"),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.CallTool(ctx, api.Actor{Identity: "agent@host"}, "issue_get",
		json.RawMessage(fmt.Sprintf(`{"id":%d}`, issue.ID)))
	if !errors.Is(err, renewErr) {
		t.Fatalf("issue_get renew error = %v, want %v", err, renewErr)
	}
}

func TestIssueCloseRejectsAReplacementIncident(t *testing.T) {
	fixture := newControlFixture(t)
	ctx := context.Background()
	first, err := fixture.store.UpsertCIIssue(
		ctx, "runner@oberth", fixture.repo.ID, "feature/stale-close", "first failure", "old incident",
	)
	if err != nil {
		t.Fatal(err)
	}
	interleaver := &issueCloseInterleaver{Store: fixture.store}
	service, err := NewAPI(APIConfig{
		Runs: fixture.store, History: fixture.store, Repositories: fixture.store,
		Issues: interleaver, Promotions: fixture.store, PromotionRuns: fixture.store,
		Enqueues: fixture.scheduler, Git: fixture.git, Logs: fixture.logs,
		Auditor: fixture.store, Signals: fixture.signals,
		PromotionWorkspaceRoot: filepath.Join(fixture.root, "stale-issue-close"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.CallTool(ctx, api.Actor{Identity: "agent@host"}, "issue_close",
		json.RawMessage(fmt.Sprintf(`{"id":%d}`, first.ID)))
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale issue close error = %v, want ErrNotFound", err)
	}
	open, err := fixture.store.OpenCIIssue(ctx, fixture.repo.ID, first.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if open.ID != interleaver.replacement.ID || open.State != model.IssueOpen {
		t.Fatalf("replacement incident = %#v, want open issue %d", open, interleaver.replacement.ID)
	}
}

func TestIssueMutationLockRenewalIsNotPostCommit(t *testing.T) {
	t.Run("update enforces and renews the lock in the store transaction", func(t *testing.T) {
		fixture := newControlFixture(t)
		ctx := context.Background()
		owner := api.Actor{Identity: "owner@host"}
		other := api.Actor{Identity: "other@host"}
		issue, err := fixture.store.CreateManualIssue(ctx, owner.Identity, model.ManualIssueSpec{Title: "original", Body: "body"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.AcquireIssueLock(ctx, issue.ID, owner.Identity); err != nil {
			t.Fatal(err)
		}
		service := fixture.api(t)

		_, err = service.CallTool(ctx, other, "issue_update",
			json.RawMessage(fmt.Sprintf(`{"id":%d,"title":"changed","body":"body"}`, issue.ID)))
		if !errors.Is(err, store.ErrLockHeld) {
			t.Fatalf("non-owner issue_update error = %v, want ErrLockHeld", err)
		}
		stored, err := fixture.store.Issue(ctx, issue.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Title != issue.Title {
			t.Fatalf("issue after rejected update = %#v", stored)
		}
		value, err := service.CallTool(ctx, owner, "issue_update",
			json.RawMessage(fmt.Sprintf(`{"id":%d,"title":"changed","body":"body"}`, issue.ID)))
		if err != nil || value.(api.IssueResponse).Title != "changed" {
			t.Fatalf("owner issue_update = %#v, %v", value, err)
		}
	})

	t.Run("close commits without post-close renewal", func(t *testing.T) {
		fixture := newControlFixture(t)
		ctx := context.Background()
		actor := api.Actor{Identity: "agent@host"}
		issue, err := fixture.store.CreateManualIssue(ctx, actor.Identity, model.ManualIssueSpec{Title: "close me", Body: "body"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.AcquireIssueLock(ctx, issue.ID, actor.Identity); err != nil {
			t.Fatal(err)
		}
		failing := &issueRenewFailer{Store: fixture.store, err: errors.New("renew must not run")}
		service, err := NewAPI(APIConfig{
			Runs: fixture.store, History: fixture.store, Repositories: fixture.store,
			Issues: failing, Promotions: fixture.store, PromotionRuns: fixture.store,
			Enqueues: fixture.scheduler, Git: fixture.git, Logs: fixture.logs,
			Auditor: fixture.store, Signals: fixture.signals,
			PromotionWorkspaceRoot: filepath.Join(fixture.root, "close-without-renew"),
		})
		if err != nil {
			t.Fatal(err)
		}

		value, err := service.CallTool(ctx, actor, "issue_close",
			json.RawMessage(fmt.Sprintf(`{"id":%d}`, issue.ID)))
		if err != nil {
			t.Fatal(err)
		}
		closed := value.(api.IssueResponse)
		if closed.State != string(model.IssueClosed) || failing.calls != 0 {
			t.Fatalf("closed issue = %#v, renew calls = %d", closed, failing.calls)
		}
		if err := fixture.store.ReleaseIssueLock(ctx, issue.ID, actor.Identity); !errors.Is(err, store.ErrLockNotOwned) {
			t.Fatalf("released close lock error = %v, want ErrLockNotOwned", err)
		}
	})
}

func TestIssueToolsListBothKindsAndUseActingIdentity(t *testing.T) {
	fixture := newControlFixture(t)
	service := fixture.api(t)
	ctx := context.Background()
	actor := api.Actor{Identity: "agent@host", Fingerprint: "SHA256:agent"}
	createdValue, err := service.CallTool(ctx, actor, "issue_create", json.RawMessage(`{"title":"manual","body":"body"}`))
	if err != nil {
		t.Fatal(err)
	}
	createdResponse := createdValue.(api.IssueCreateResponse)
	created, err := fixture.store.Issue(ctx, createdResponse.ID)
	if err != nil {
		t.Fatal(err)
	}
	if created.RepoID != 0 {
		t.Fatalf("manual issue repository = %d, want workspace-global", created.RepoID)
	}
	if _, err := service.CallTool(ctx, actor, "issue_create", json.RawMessage(`{"repo":"oberth","title":"extra","body":"body"}`)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("non-FAB issue_create field error = %v", err)
	}
	if _, err := service.CallTool(ctx, actor, "issue_create", json.RawMessage(`{"title":"missing body"}`)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing issue body error = %v", err)
	}
	ciIssue, err := fixture.store.UpsertCIIssue(ctx, "runner@oberth", fixture.repo.ID, "feature/red", "CI red", "failed")
	if err != nil {
		t.Fatal(err)
	}
	lockedValue, err := service.CallTool(ctx, actor, "issue_lock", json.RawMessage(fmt.Sprintf(`{"id":%d}`, created.ID)))
	if err != nil {
		t.Fatal(err)
	}
	locked := lockedValue.(api.IssueLockResponse)
	if locked.ID != created.ID || locked.Owner != actor.Identity || locked.ExpiresAt == "" {
		t.Fatalf("issue lock response = %#v", locked)
	}
	gotValue, err := service.CallTool(ctx, actor, "issue_get", json.RawMessage(fmt.Sprintf(`{"id":%d}`, created.ID)))
	if err != nil {
		t.Fatal(err)
	}
	got := gotValue.(api.IssueResponse)
	if got.ID != created.ID || got.Kind != string(model.IssueManual) || got.Title != "manual" || got.Body != "body" || got.State != string(model.IssueOpen) {
		t.Fatalf("issue get response = %#v", got)
	}
	if _, err := service.CallTool(ctx, actor, "issue_update", json.RawMessage(fmt.Sprintf(`{"id":%d,"title":"partial"}`, created.ID))); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("partial issue_update error = %v", err)
	}
	updatedValue, err := service.CallTool(ctx, actor, "issue_update", json.RawMessage(fmt.Sprintf(`{"id":%d,"title":"updated","body":"new"}`, created.ID)))
	if err != nil {
		t.Fatal(err)
	}
	updated := updatedValue.(api.IssueResponse)
	if updated.Title != "updated" || updated.Body != "new" {
		t.Fatalf("issue update response = %#v", updated)
	}
	closedValue, err := service.CallTool(ctx, actor, "issue_close", json.RawMessage(fmt.Sprintf(`{"id":%d}`, created.ID)))
	if err != nil {
		t.Fatal(err)
	}
	closed := closedValue.(api.IssueResponse)
	if closed.State != string(model.IssueClosed) {
		t.Fatalf("issue close response = %#v", closed)
	}
	listedValue, err := service.CallTool(ctx, actor, "issue_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	page := listedValue.(api.IssueListResponse)
	if len(page.Issues) != 2 {
		t.Fatalf("unified issue page = %#v", page)
	}
	listed := map[int64]string{}
	for _, issue := range page.Issues {
		listed[issue.ID] = issue.State
	}
	if listed[created.ID] != string(model.IssueClosed) || listed[ciIssue.ID] != string(model.IssueOpen) {
		t.Fatalf("issue list response = %#v", page)
	}
	if _, err := service.CallTool(ctx, actor, "issue_list", json.RawMessage(`{"limit":50}`)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("non-FAB issue_list field error = %v", err)
	}
	if _, err := service.CallTool(ctx, actor, "issue_delete", json.RawMessage(fmt.Sprintf(`{"id":%d}`, created.ID))); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CallTool(ctx, actor, "issue_get", json.RawMessage(fmt.Sprintf(`{"id":%d}`, created.ID))); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted issue lookup error = %v", err)
	}
}
