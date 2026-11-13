package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"log"

	"github.com/oberthci/oberth/internal/gitoid"
	"github.com/oberthci/oberth/internal/liveness"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

const maximumCIIssueTailBytes = 16 << 10
const mutationGateRetryInterval = time.Second

var errMutationBlocked = errors.New("service: mutation blocked by unavailable audit integrity")

type mutationRequirement func(context.Context) error

type SchedulerConfig struct {
	Store           SchedulerStore
	Git             WorkspaceGit
	Logs            LogStore
	Jobs            JobController
	ReleaseJobs     ReleaseJobController
	Auditor         Auditor
	Signals         *Signals
	WorkspaceRoot   string
	RemoveWorkspace func(string) error
	MaxRunLogBytes  int64
	MutationGate    func(context.Context) error

	// RestoredRunsFile is oberth restore's marker (RestoredRunsFile in the
	// data root); empty disables it.
	RestoredRunsFile string
}

type Scheduler struct {
	restoredRunsFile string

	databaseRetryCount atomic.Uint64
	dispatchProgress   liveness.Heartbeat
	startupRecovered   atomic.Bool
	store              SchedulerStore
	git                WorkspaceGit
	logs               LogStore
	jobs               JobController
	releaseJobs        ReleaseJobController
	auditor            Auditor
	signals            *Signals
	workspaces         *workspaceLifecycle
	workspaceRoot      string
	submissions        *submissionRegistry

	maxRunLogBytes int64

	mutationGate       func(context.Context) error
	wake               chan struct{}
	publicationWake    chan struct{}
	evidenceMu         sync.Mutex
	evidenceExecutions map[string]int
	publicationMu      sync.Mutex
	publicationIDs     []string
	publicationSet     map[string]struct{}
	deliveryPermit     chan struct{}
	policyRetryWait    func(context.Context) error
	policyWaitTimeout  time.Duration // tests may shorten the fixed production limit
	// deferredJournals holds the pod-log journal of a run whose worker left
	// its Workflow Stop to the dispatch pass; that pass prunes it.
	deferredJournalMu sync.Mutex
	deferredJournals  map[string]string
	// promotionRetrier re-promotes a promotion whose delivery found its
	// target moved (#1097); nil leaves such a promotion failed.
	promotionRetrier atomic.Pointer[promotionRetrierHook]
	// onStartupRecovered, when set (tests only), is called once Run has
	// finished startup recovery and is about to enter its dispatch loop.
	onStartupRecovered func()
	dispatchTicks      <-chan time.Time // test-only idle polling clock
	createMu           sync.Mutex
	createPending      map[string]*pendingCreate
	createWake         chan struct{}
	recoveredWorkflows chan adoptedRun
	createNow          func() time.Time // test-only reconciliation clock
	createObservers    sync.WaitGroup
	createTicks        <-chan time.Time // test-only reconciliation ticks
	// onDispatchIdle, when set (tests only), observes the completed dispatch
	// iteration immediately before its idle select.
	onDispatchIdle func()
}

// PromotionRetrier re-promotes a promotion whose publication failed because
// its target moved past the base it was merged onto. The API implements it.
type PromotionRetrier interface {
	RetryMovedPromotion(context.Context, model.Promotion)
}

type promotionRetrierHook struct{ retrier PromotionRetrier }

// SetPromotionRetrier registers the moved-target retry hook. It is called
// after the delivery permit is released, so the retry may deliver itself.
func (scheduler *Scheduler) SetPromotionRetrier(retrier PromotionRetrier) {
	scheduler.promotionRetrier.Store(&promotionRetrierHook{retrier: retrier})
}

func (scheduler *Scheduler) retryMovedPromotion(ctx context.Context, finalization model.PublicationFinalization) {
	hook := scheduler.promotionRetrier.Load()
	if hook == nil || hook.retrier == nil || finalization.Promotion.ID == "" ||
		finalization.Promotion.Status != model.PromotionFailed ||
		finalization.Publication.Status != model.PublicationFailed || finalization.TargetMovedTo == "" {
		return
	}
	hook.retrier.RetryMovedPromotion(withoutDispatchProgress(ctx), finalization.Promotion)
}

func NewScheduler(config SchedulerConfig) (*Scheduler, error) {

	if config.Store == nil || config.Git == nil || config.Logs == nil || config.Jobs == nil || config.Auditor == nil {
		return nil, errors.New("service: scheduler store, Git, logs, jobs, and auditor are required")
	}
	if config.MaxRunLogBytes <= 0 {
		config.MaxRunLogBytes = runlog.DefaultRunLimitBytes
	}
	if strings.TrimSpace(config.WorkspaceRoot) == "" {
		config.WorkspaceRoot = "/data/work"
	}
	workspaceRoot := filepath.Clean(config.WorkspaceRoot)
	workspaces, err := newWorkspaceLifecycle(workspaceRoot, config.Store, config.RemoveWorkspace)
	if err != nil {
		return nil, err
	}
	signals := config.Signals
	if signals == nil {
		signals = NewSignals()
	}
	mutationGate := config.MutationGate
	if mutationGate == nil {
		mutationGate = func(context.Context) error { return nil }
	}
	deliveryPermit := make(chan struct{}, 1)
	deliveryPermit <- struct{}{}
	scheduler := &Scheduler{

		store: config.Store, git: config.Git, logs: config.Logs, jobs: config.Jobs,
		releaseJobs: config.ReleaseJobs,
		auditor:     config.Auditor, signals: signals,
		workspaces: workspaces, workspaceRoot: workspaceRoot, restoredRunsFile: config.RestoredRunsFile,
		submissions:    newSubmissionRegistry(),
		maxRunLogBytes: config.MaxRunLogBytes, mutationGate: mutationGate,
		wake: make(chan struct{}, 1), publicationWake: make(chan struct{}, 1),
		recoveredWorkflows: make(chan adoptedRun, 1), createWake: make(chan struct{}, 1), createPending: make(map[string]*pendingCreate),
		publicationSet: make(map[string]struct{}), deliveryPermit: deliveryPermit, policyRetryWait: waitPolicyRetry,
	}
	scheduler.workspaces.owners = scheduler.database()
	// Construction precedes owner recovery at activation. Hand marker-listed
	// rows to restore cancellation before the store can fail unnamed claims.
	restored, err := loadRestoredRuns(config.RestoredRunsFile)
	if err != nil {
		return nil, err
	}
	if owner, ok := config.Store.(interface{ PreserveRestoredRuns(map[string]bool) }); ok {
		owner.PreserveRestoredRuns(restored)
	} else if len(restored) != 0 {
		return nil, errors.New("service: store cannot preserve restored runs for owner recovery")
	}
	return scheduler, nil
}

func (scheduler *Scheduler) EnqueueCI(ctx context.Context, request CIRequest) (model.EnqueueRunResult, error) {
	result, err := scheduler.enqueueCI(ctx, request, nil)
	return result.Enqueued, err
}

// EnqueueScheduledCI is the cron-only path: cursor consumption, active-entry
// dedupe, receive attribution and queue insertion commit atomically.
func (scheduler *Scheduler) EnqueueScheduledCI(ctx context.Context, request CIRequest, fire model.ScheduleFireSpec) (model.ScheduleEnqueueResult, error) {
	return scheduler.enqueueCI(ctx, request, &fire)
}

type scheduleOccurrenceStore interface {
	EnqueueScheduleOccurrence(context.Context, model.ReceiveEventSpec, model.RunSpec, model.ScheduleFireSpec) (model.ScheduleEnqueueResult, error)
}

func (scheduler *Scheduler) enqueueCI(ctx context.Context, request CIRequest, fire *model.ScheduleFireSpec) (model.ScheduleEnqueueResult, error) {
	if err := scheduler.requireMutation(ctx); err != nil {
		return model.ScheduleEnqueueResult{}, err
	}
	if strings.TrimSpace(request.EventID) == "" || request.Repository.ID <= 0 || strings.TrimSpace(request.Branch) == "" ||
		!validOID(request.SHA) || strings.TrimSpace(request.Actor) == "" {
		return model.ScheduleEnqueueResult{}, fmt.Errorf("%w: CI request is invalid", ErrInvalidInput)
	}
	testedSHA := request.TestedSHA
	if testedSHA == "" {
		testedSHA = request.SHA
	}
	trigger := request.Trigger
	if trigger == "" {
		trigger = "branch"
	}
	kind := request.RefKind
	if kind == "" {
		kind = model.RefBranch
	}
	if !kind.Valid() || ((kind == model.RefTag) != (trigger == "tag")) || (kind == model.RefTag && fire != nil) {
		return model.ScheduleEnqueueResult{}, fmt.Errorf("%w: CI ref kind and trigger differ", ErrInvalidInput)
	}
	event := receiveSpec(request.EventID, request.Actor, request.Repository.ID, kind, request.Branch,
		request.OldSHA, request.SHA, testedSHA, "queued")
	spec := model.RunSpec{

		Assist: request.Assist,
		Reason: request.Reason,
		RepoID: request.Repository.ID, RefKind: kind, Ref: request.Branch,
		SHA: request.SHA, Actor: request.Actor, Trigger: trigger, ScheduleEntry: request.ScheduleEntry, TestedSHA: testedSHA, BaseSHA: request.BaseSHA,
	}
	var result model.ScheduleEnqueueResult
	var err error
	if fire != nil {
		state, ok := scheduler.store.(scheduleOccurrenceStore)
		if !ok {
			return result, errors.New("service: atomic schedule enqueue is unavailable")
		}
		result, err = state.EnqueueScheduleOccurrence(ctx, event, spec, *fire)
	} else {
		result.Enqueued, err = scheduler.database().EnqueueReceiveEvent(ctx, event, spec)
	}
	if err == nil && result.Enqueued.ID != "" {
		err = scheduler.AcceptEnqueue(ctx, result.Enqueued)
	}
	return result, err
}

func (scheduler *Scheduler) AdmitRelease(ctx context.Context, request ReleaseRequest) (model.EnqueueRunResult, error) {
	if err := scheduler.requireMutation(ctx); err != nil {
		return model.EnqueueRunResult{}, err
	}
	if strings.TrimSpace(request.EventID) == "" || request.Repository.ID <= 0 || strings.TrimSpace(request.Tag) == "" ||
		!validOID(request.ObjectSHA) || !validOID(request.CommitSHA) || strings.TrimSpace(request.Actor) == "" {
		return model.EnqueueRunResult{}, fmt.Errorf("%w: release request is invalid", ErrInvalidInput)
	}
	event := receiveSpec(request.EventID, request.Actor, request.Repository.ID, model.RefTag, request.Tag,
		request.OldSHA, request.ObjectSHA, request.CommitSHA, "queued")
	enqueued, err := scheduler.database().EnqueueReceiveEvent(ctx, event, model.RunSpec{

		RepoID: request.Repository.ID, RefKind: model.RefTag, Ref: request.Tag,
		SHA: request.ObjectSHA, Actor: request.Actor, Release: true, Trigger: "tag", TestedSHA: request.CommitSHA,
	})
	if err != nil {
		return model.EnqueueRunResult{}, err
	}
	if err := scheduler.AcceptEnqueue(ctx, enqueued); err != nil {
		return enqueued, err
	}
	return enqueued, nil
}

func (scheduler *Scheduler) AcceptEnqueue(context.Context, model.EnqueueRunResult) error {
	scheduler.signalQueue()
	return nil
}

// AcceptCancellations executes the obligations an explicit run cancel
// recorded: a waiting run's admission reservation is withdrawn at once and a
// claimed run's Workflow is stopped (an absent one is fine). A failed Stop
// stays pending for the cancellation pass, until Argo finishes.
//
// AI-CONTRACT: jobs.Stop runs synchronously on the caller's context, which
// for run_cancel is the MCP request. A client disconnect or request timeout
// cancels that context, so Stop fails and the obligation stays
// pending; the cancel itself is already committed, and the scheduler's next
// cancellation pass (queue wake or restart) retries Stop.
func (scheduler *Scheduler) AcceptCancellations(ctx context.Context, cancellations []model.RunCancellation) error {
	err := scheduler.completeCancellations(ctx, cancellations)
	scheduler.signalQueue()
	return err
}

// NotifyQueue wakes a long-running scheduler after a state transition makes
// previously gated work eligible without enqueuing a new run. Fast-forward
// promotion publication is the primary caller.
func (scheduler *Scheduler) NotifyQueue() {
	scheduler.signalQueue()
}

// NotifyPublicationRecovery wakes the durable publication outbox without
// making ordinary queue activity race an API-owned delivery attempt.
func (scheduler *Scheduler) NotifyPublicationRecovery(publicationID string) {
	if strings.TrimSpace(publicationID) == "" {
		return
	}
	scheduler.publicationMu.Lock()
	if _, exists := scheduler.publicationSet[publicationID]; !exists {
		scheduler.publicationSet[publicationID] = struct{}{}
		scheduler.publicationIDs = append(scheduler.publicationIDs, publicationID)
	}
	scheduler.publicationMu.Unlock()
	select {
	case scheduler.publicationWake <- struct{}{}:
	default:
	}
}

func (scheduler *Scheduler) completePendingCancellationsWithGate(ctx context.Context, require mutationRequirement) error {
	pending, err := scheduler.database().PendingRunCancellations(ctx)
	if err != nil {
		return fmt.Errorf("load pending run cancellations: %w", err)
	}
	err = scheduler.completeCancellationsWithGate(ctx, pending, require)
	return err
}

func (scheduler *Scheduler) completeCancellations(ctx context.Context, cancellations []model.RunCancellation) error {
	return scheduler.completeCancellationsWithGate(ctx, cancellations, scheduler.requireMutation)
}

func (scheduler *Scheduler) completeCancellationsWithGate(ctx context.Context, cancellations []model.RunCancellation, require mutationRequirement) error {
	// A run that is cancelled while it is still preparing or waiting holds
	// in-memory submission ownership but may own no Workflow. Withdraw that
	// unsubmitted ownership first, independently of the audit gate. Durable
	// cancellation cleanup still verifies the original Workflow identity.
	scheduler.cancelCreatingRuns(ctx, cancellations)
	for _, cancellation := range cancellations {
		forgetActiveLogIndex(scheduler.logs, cancellation.RunID)
	}
	if err := require(ctx); err != nil {
		return err
	}
	// Supplied snapshots can predate a lost-acknowledgement re-arm. Use the
	// current ledger entry and compare it again when completing the pass.
	pending, err := scheduler.database().PendingRunCancellations(ctx)
	if err != nil {
		return err
	}
	currentCancellations := make(map[string]model.RunCancellation, len(pending))
	for _, current := range pending {
		currentCancellations[current.RunID] = current
	}
	var deletions []error
	for _, cancellation := range cancellations {
		beatDispatch(ctx)
		current, pending := currentCancellations[cancellation.RunID]
		if !pending {
			continue
		}
		cancellation = current
		run, err := scheduler.database().Run(ctx, cancellation.RunID)
		if err != nil {
			return err
		}
		// The cancellation ledger owns the Workflow until settlement. An
		// empty name can still belong to a live checkout.
		if cancellation.WorkflowName == "" {
			// The live owner may still be checking out. Only worker join or
			// owner restart can confirm absence and release this workspace.
			continue
		}
		if cancellation.Terminate {
			if err := runDispatchExternal(ctx, func(ctx context.Context) error {
				return scheduler.jobs.Terminate(ctx, cancellation.WorkflowName, cancellation.RunID)
			}); err != nil {
				scheduler.registerCreateCleanup(run, isPendingCreate(run) || run.Status.Terminal())
				deletions = append(deletions, &pendingWorkflowStopError{err})
				continue
			}
		}
		if isPendingCreate(run) || scheduler.ownsCreateCleanup(run.ID) || run.Status.Terminal() {
			scheduler.registerCreateCleanup(run, isPendingCreate(run) || run.Status.Terminal())
			continue // Never wait for Kubernetes on dispatch/push/cancel.
		}
		scheduler.submissions.mu.Lock()
		owner := scheduler.submissions.owners[run.QueueSequence]
		unobserved := owner == nil && (scheduler.startupRecovered.Load() || run.Cancelled())
		scheduler.submissions.mu.Unlock()
		if err := runDispatchExternal(ctx, func(ctx context.Context) error {
			if cancellation.Terminate {
				return nil
			}
			return scheduler.jobs.Stop(ctx, cancellation.WorkflowName, cancellation.RunID)
		}); err != nil {
			if run.Status == model.RunRunning && unobserved {
				scheduler.registerCreateCleanup(run, false)
			}
			// The obligation stays pending. Keep going: one Workflow the API
			// would not stop must not hold back the others in this pass.
			deletions = append(deletions, fmt.Errorf("Stop Workflow %s: %w", cancellation.WorkflowName, &pendingWorkflowStopError{err}))
			continue
		}
		if run.Status == model.RunRunning && unobserved {
			scheduler.registerCreateCleanup(run, false)
			continue
		}
		// Stop acceptance leaves the Workflow and onExit alive. The observer
		// records Argo's phase before this obligation can be retired.
		settled := run.Status.Terminal()
		if run.Status == model.RunQueued {
			// A restart intent may refer to the prior Workflow while its run
			// is queued. Stop acceptance cannot permit its next submission.
			if settler, ok := scheduler.jobs.(JobCreateSettler); ok {
				settled, err = settler.SettleCreate(ctx, cancellation.WorkflowName, run.ID, cancellation.CreatedAt.Add(createObservationWindow))
				if err != nil {
					return err
				}
			}
		}
		if !settled {
			continue
		}
		if err := scheduler.database().CompleteObservedRunCancellation(ctx, cancellation); err != nil {
			return fmt.Errorf("complete cancellation for run %s: %w", cancellation.RunID, err)
		}
		scheduler.pruneDeferredPodJournal(ctx, cancellation.RunID)
		cleanupCtx, cancelCleanup := boundedWorkspaceCleanupContext(ctx)
		cleanupErr := scheduler.workspaces.cleanupRunDeferred(cleanupCtx, cancellation.RunID)
		cancelCleanup()
		if cleanupErr != nil {
			return fmt.Errorf("clean canceled run workspace %s: %w", cancellation.RunID, cleanupErr)
		}
	}
	return errors.Join(deletions...)
}

// cancelCreatingRuns wakes creates whose durable cancellation has terminalized the run.
func (scheduler *Scheduler) cancelCreatingRuns(ctx context.Context, cancellations []model.RunCancellation) {
	for _, cancellation := range cancellations {
		run, err := scheduler.database().Run(ctx, cancellation.RunID)
		if err != nil || (!run.Status.Terminal() && !run.Cancelled()) {
			continue
		}
		scheduler.submissions.abandon(run.QueueSequence)
	}
}

// Recovered reports that durable owner recovery and Workflow adoption have
// completed. It is safe to use from readiness probes without store access.
func (scheduler *Scheduler) Recovered() bool { return scheduler.startupRecovered.Load() }

// Run owns oldest-eligible claims for one server process and submits each
// claimed run's Workflow at once; the Argo controller's parallelism postpones
// the excess. While a publication is pending it gates only its exact
// repository/ref; queue changes and completions wake the scheduler through
// bounded channels.
func (scheduler *Scheduler) Run(ctx context.Context) error {
	workerCtx, cancelWorkers := context.WithCancel(ctx)
	workerCtx = context.WithValue(workerCtx, databaseRetryKey{}, &databaseRetryScope{lifetime: workerCtx, owner: scheduler})
	defer cancelWorkers()
	var workers sync.WaitGroup
	join := func() {
		cancelWorkers()
		workers.Wait()
	}
	if err := scheduler.awaitMutation(workerCtx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	var adopted []adoptedRun
	defer func() {
		for _, run := range adopted {
			run.reservation.cancel()
		}
	}()
	if err := scheduler.reconcileStrandedRunsWithGate(workerCtx, scheduler.awaitMutation, func(run model.Run) error {
		reservation, err := scheduler.submissions.adopt(run)
		if err != nil {
			return err
		}
		adopted = append(adopted, adoptedRun{run: run, reservation: reservation})
		return nil
	}); err != nil {
		return err
	}
	if err := scheduler.waitForStartupCleanup(workerCtx, "cancellations", func(ctx context.Context) error {
		return scheduler.completePendingCancellationsWithGate(ctx, scheduler.awaitMutation)
	}); err != nil {
		return err
	}
	if err := scheduler.recoverPendingPublicationsWithGate(workerCtx, scheduler.awaitMutation); err != nil {
		return err
	}
	if err := scheduler.recoverOwnedWorkspaces(workerCtx); err != nil {
		return err
	}
	scheduler.dispatchProgress.Beat()
	dispatchCtx := withDispatchProgress(workerCtx, &scheduler.dispatchProgress)
	workers.Add(1)
	go func() { defer workers.Done(); scheduler.sweepWorkspaces(workerCtx) }()
	scheduler.startupRecovered.Store(true)
	defer scheduler.startupRecovered.Store(false)
	if scheduler.onStartupRecovered != nil {
		scheduler.onStartupRecovered()
	}
	scheduler.signalQueue()
	workers.Add(1)
	go func() { defer workers.Done(); scheduler.runCreateCleanup(workerCtx) }()
	completed := make(chan error, max(len(adopted), 8))
	for _, adopted := range adopted {
		workers.Add(1)
		go func() {
			defer workers.Done()
			err := scheduler.resumeAdoptedRun(workerCtx, adopted.run, scheduler.awaitMutation, adopted.reservation)
			select {
			case completed <- err:
			case <-workerCtx.Done():
			}
		}()
	}
	// A Workflow Stop the Kubernetes API refused leaves its cancellation
	// obligation pending. That is an availability blip, not a reason to end
	// the server and drop every SSH session and log stream: log it and run
	// the pass again after a bounded backoff. Store errors stay fatal.
	var cancellationRetry <-chan time.Time
	cancellationDelay := databaseRetryMinimum
	deletionRetries := 0
	cancellationPass := func() error {
		err := scheduler.completePendingCancellationsWithGate(dispatchCtx, scheduler.awaitDispatchMutation)
		if err == nil || !onlyPendingWorkflowStops(err) || ctx.Err() != nil {
			if err == nil {
				cancellationDelay = databaseRetryMinimum
			}
			return err
		}
		deletionRetries++
		log.Printf("WARN: scheduler cancellation pass deletion_pending=true deletion_retries_total=%d retry_in=%s error=%q", deletionRetries, cancellationDelay, err)
		if cancellationRetry == nil {
			cancellationRetry = time.After(cancellationDelay)
		}
		cancellationDelay = min(2*cancellationDelay, cancellationRetryMaximum)
		return nil
	}
	submissionPoll := time.NewTicker(submissionPollInterval)
	defer submissionPoll.Stop()
	ticks := submissionPoll.C
	if scheduler.dispatchTicks != nil {
		ticks = scheduler.dispatchTicks
	}
	for {
		scheduler.dispatchProgress.Beat()
		for {
			scheduler.dispatchProgress.Beat()
			run, err := scheduler.claimDispatchRun(dispatchCtx)
			if store.IsTransientLockError(err) {
				break
			}
			if queueEmpty(err) {
				break
			}
			if err != nil {
				join()
				if ctx.Err() != nil || errors.Is(err, context.Canceled) {
					return nil
				}
				return fmt.Errorf("claim accepted run: %w", err)
			}
			reservation := scheduler.submissions.trackRun(run)
			workers.Add(1)
			go func(run model.Run, reservation *submissionOwner) {
				defer workers.Done()
				err := scheduler.executeAndCleanupTrackedWithGate(workerCtx, run, scheduler.awaitMutation, reservation)
				select {
				case completed <- err:
				case <-workerCtx.Done():
				}
			}(run, reservation)
		}
		if scheduler.onDispatchIdle != nil {
			scheduler.onDispatchIdle()
		}
		select {
		case <-ticks:
			if err := scheduler.recoverNotifiedPublicationsWithGate(dispatchCtx, scheduler.awaitDispatchMutation); err != nil {
				join()
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		case <-ctx.Done():
			join()
			return nil
		case <-scheduler.wake:
			if err := cancellationPass(); err != nil {
				join()
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		case <-cancellationRetry:
			cancellationRetry = nil
			if err := cancellationPass(); err != nil {
				join()
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		case <-scheduler.publicationWake:
			if err := scheduler.recoverNotifiedPublicationsWithGate(dispatchCtx, scheduler.awaitDispatchMutation); err != nil {
				join()
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			scheduler.signalQueue()
		case recovered := <-scheduler.recoveredWorkflows:
			workers.Add(1)
			go func() {
				defer workers.Done()
				err := scheduler.resumeAdoptedRun(workerCtx, recovered.run, scheduler.awaitMutation, recovered.reservation)
				select {
				case completed <- err:
				case <-workerCtx.Done():
				}
			}()
		case err := <-completed:
			if err != nil {
				join()
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
}

// ProcessNext is a deterministic single-run entrypoint for recovery tools and
// focused tests. The long-running server normally uses Run.
func (scheduler *Scheduler) ProcessNext(ctx context.Context) error {
	if err := scheduler.requireMutation(ctx); err != nil {
		return err
	}
	run, err := scheduler.database().ClaimNextRun(ctx)
	if err != nil {
		return err
	}
	return scheduler.executeAndCleanup(ctx, run)
}

func (scheduler *Scheduler) executeAndCleanup(ctx context.Context, run model.Run) error {
	return scheduler.executeAndCleanupWithGate(ctx, run, scheduler.requireMutation)
}

func (scheduler *Scheduler) executeAndCleanupWithGate(ctx context.Context, run model.Run, require mutationRequirement) error {
	reservation := scheduler.submissions.trackRun(run)
	return scheduler.executeAndCleanupTrackedWithGate(ctx, run, require, reservation)
}

func (scheduler *Scheduler) executeAndCleanupTrackedWithGate(
	ctx context.Context,
	run model.Run,
	require mutationRequirement,
	reservation *submissionOwner,
) error {
	defer reservation.cancel()
	finishEvidence := scheduler.beginEvidenceExecution(run.ID)
	defer finishEvidence()
	// A cancellation can terminalize a claim before its goroutine starts. Do
	// not create logs after a purge that completed in that dispatch window.
	current, executeErr := scheduler.database().Run(ctx, run.ID)
	if executeErr == nil && current.Status.Active() {
		executeErr = scheduler.execute(ctx, run, require, reservation)
	}
	if errors.Is(executeErr, errMutationBlocked) {
		return executeErr
	}
	cleanupCtx, cancelCleanup := boundedWorkspaceCleanupContext(ctx)
	defer cancelCleanup()
	current, readErr := scheduler.database().Run(cleanupCtx, run.ID)
	if readErr != nil {
		return errors.Join(executeErr, readErr)
	}
	if current.Status.Active() && current.Cancelled() && !scheduler.ownsCreateCleanup(run.ID) {
		if current.WorkflowName == "" {
			_, finishErr := scheduler.database().FinishRun(cleanupCtx, run.ID, model.RunResult{Status: model.RunInterrupted, Phase: model.PhaseCancelled, Error: strings.Replace(current.Reason, "cancelling by ", "cancelled by ", 1)})
			if finishErr != nil {
				return errors.Join(executeErr, finishErr)
			}
			scheduler.signals.NotifyRun(run.ID)
		} else {
			scheduler.registerCreateCleanup(current, false)
		}
	}
	if workflowRunAdoptable(run) {
		current, err := scheduler.database().Run(cleanupCtx, run.ID)
		if err != nil {
			return errors.Join(executeErr, err)
		}
		if current.Status == model.RunRunning && current.WorkflowName != "" {
			return executeErr
		}
	}
	cleanupErr := scheduler.workspaces.cleanupRunDeferred(cleanupCtx, run.ID)
	if run.Trigger == "promotion" {
		promotion, err := scheduler.database().PromotionByRun(cleanupCtx, run.ID)
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("load promotion workspace owner for run %s: %w", run.ID, err))
		} else {
			cleanupErr = errors.Join(cleanupErr, scheduler.workspaces.cleanupPromotionDeferred(cleanupCtx, promotion.ID))
		}
	}
	return errors.Join(executeErr, cleanupErr)
}

// recoverOwnedWorkspaces removes only exact database-owned terminal paths at
// startup. It never scans the workspace root, so active ingress and
// unrecognized paths are outside its deletion surface.
func (scheduler *Scheduler) recoverOwnedWorkspaces(ctx context.Context) error {
	if err := scheduler.workspaces.recover(ctx); err != nil {
		return fmt.Errorf("recover terminal workspaces: %w", err)
	}
	return nil
}

func (scheduler *Scheduler) execute(ctx context.Context, run model.Run, require mutationRequirement, reservation *submissionOwner) error {
	if err := require(ctx); err != nil {
		return err
	}
	repository, err := scheduler.database().Repository(ctx, run.RepoID)
	if err != nil {
		return scheduler.finishInfrastructureFailure(ctx, run, model.Repository{}, fmt.Errorf("load run repository: %w", err))
	}
	checkoutSHA := run.TestedSHA
	if checkoutSHA == "" {
		checkoutSHA = run.SHA
	}
	sourceDir := filepath.Join(scheduler.workspaceRoot, run.ID, "src")
	if !pathWithin(scheduler.workspaceRoot, sourceDir) {
		return scheduler.finishInfrastructureFailure(ctx, run, repository, errors.New("workspace path escaped root"))
	}
	// The cache input is fully qualified so same-named repositories under
	// different upstreams check out from their own caches (issue #264).
	upstream, err := scheduler.database().Upstream(ctx, repository.UpstreamID)
	if err != nil {
		return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("load run repository upstream: %w", err))
	}
	// Only fresh execution initializes empty evidence. Recovery/adoption and
	// create settlement never manufacture an empty journal for missing history.
	if run.WorkflowName == "" && run.StartedAt == nil {
		if initializer, ok := scheduler.logs.(interface{ InitializeStepProgress(string) error }); ok {
			if err := initializer.InitializeStepProgress(run.ID); err != nil {
				return scheduler.finishInfrastructureFailure(ctx, run, repository, err)
			}
		}
	}
	if err := scheduler.git.Checkout(ctx, upstream.QualifiedRepo(repository.Name), checkoutSHA, sourceDir); err != nil {
		return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("checkout run workspace: %w", err))
	}
	scheduler.timingMilestone(ctx, run.ID, "checkout-ready")
	if err := scheduler.recordRunIssueTrailers(ctx, run, upstream.QualifiedRepo(repository.Name), require); err != nil {
		return err
	}
	stopped, err := scheduler.awaitWorkflowPolicy(ctx, upstream.QualifiedRepo(repository.Name), run.ID, checkoutSHA, require)
	if errors.Is(err, errMutationBlocked) {
		return err
	}
	if stopped {
		return nil
	}
	if err != nil {
		return scheduler.finishInfrastructureFailure(ctx, run, repository, err)
	}
	logFile, err := scheduler.logs.Create(run.ID)
	if err != nil {
		return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("create run log: %w", err))
	}
	workflowName, err := deterministicWorkflowName(repository.Name, run.ID)
	if err != nil {
		_ = logFile.Close()
		return scheduler.finishInfrastructureFailure(ctx, run, repository, err)
	}
	current, err := scheduler.database().SetRunWorkflowName(ctx, run.ID, workflowName)
	if err != nil {
		_ = logFile.Close()
		return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("persist deterministic Job intent: %w", err))
	}
	if current.Cancelled() {
		_ = logFile.Close()
		if err := scheduler.completePendingCancellationsWithGate(ctx, require); err != nil {
			return scheduler.deferRefusedStops(ctx, run.ID, err)
		}
		scheduler.signals.NotifyRun(run.ID)
		return nil
	}
	jobRequest := WorkflowRequest{WorkflowName: workflowName, Run: run, Repository: repository, SourceDir: sourceDir, UpstreamName: upstream.Name, UpstreamOrg: upstream.Org()}
	receipt, err := scheduler.resolvePipelineSource(ctx, jobRequest)
	if err != nil {
		_ = logFile.Close()
		return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("read pipeline source receipt: %w", err))
	}
	jobRequest.SourceReceipt = receipt
	if err := require(ctx); err != nil {
		_ = logFile.Close()
		return err
	}
	current, err = scheduler.database().MarkRunSubmissionStarted(ctx, run.ID)
	if err != nil {
		_ = logFile.Close()
		return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("record submission start: %w", err))
	}
	if current.Cancelled() {
		_ = logFile.Close()
		if err := scheduler.completePendingCancellationsWithGate(ctx, require); err != nil {
			return scheduler.deferRefusedStops(ctx, run.ID, err)
		}
		scheduler.signals.NotifyRun(run.ID)
		return nil
	}
	jobRequest.Run = current
	{
		upstream, upstreamErr := scheduler.database().Upstream(ctx, repository.UpstreamID)
		if upstreamErr != nil {
			_ = logFile.Close()
			return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("load repository upstream: %w", upstreamErr))
		}
		jobRequest.UpstreamName = upstream.Name
		jobRequest.UpstreamOrg = upstream.Org()
	}
	if err := scheduler.checkReleasePreflight(ctx, jobRequest, logFile); err != nil {
		_ = logFile.Close()
		return scheduler.finishInfrastructureFailure(ctx, run, repository, err)
	}

	if err = require(ctx); err != nil {
		_ = logFile.Close()
		return err
	}
	if current.Status != model.RunRunning {
		_ = logFile.Close()
		return nil
	}
	// A run was last read before starting submission. Re-read
	// it so a cancel that landed since stops here, before any
	// Workflow exists, instead of after MarkRunSubmitted.
	if fresh, err := scheduler.database().Run(ctx, run.ID); err != nil {
		_ = logFile.Close()
		return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("verify run before Job creation: %w", err))
	} else if fresh.Cancelled() {
		_ = logFile.Close()
		if err := scheduler.completePendingCancellationsWithGate(ctx, require); err != nil {
			return scheduler.deferRefusedStops(ctx, run.ID, err)
		}
		scheduler.signals.NotifyRun(run.ID)
		return nil
	}

	if _, ok := scheduler.jobs.(JobCreateSettler); ok {
		if err := scheduler.recordWorkflowCreateBinding(run, workflowName); err != nil {
			_ = logFile.Close()
			return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("record Workflow admission contract: %w", err))
		}
	}

	creationCtx, cancelCreation := reservation.creationContext(ctx)
	defer cancelCreation()

	switch {
	case run.Release:
		if scheduler.releaseJobs == nil {
			_ = logFile.Close()
			return scheduler.finishInfrastructureFailure(ctx, run, repository, errors.New("release Job capability is unavailable"))
		}
		err = scheduler.releaseJobs.CreateRelease(creationCtx, jobRequest)
	default:
		err = scheduler.jobs.CreateWorkflow(creationCtx, jobRequest)
	}
	cancelCreation()
	if err != nil {
		var uncertain interface{ AmbiguousCreate() bool }
		if errors.As(err, &uncertain) && uncertain.AmbiguousCreate() {
			_ = logFile.Close()
			scheduler.deferCreateCleanup(ctx, run, workflowName, err)
			return nil // The reconciler settles the uncertain create.
		}
		if _, ok := scheduler.jobs.(JobAdopter); ok && workflowRunAdoptable(run) && ctx.Err() != nil {
			return logFile.Close()
		}
		_ = logFile.Close()
		// Definite rejection never deletes a foreign name. Ambiguous POSTs stay
		// owned by the settlement task above until absence is confirmed.
		return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("create Job: %w", err))
	}
	current, err = scheduler.database().MarkRunSubmitted(ctx, run.ID)
	if err != nil {
		if _, ok := scheduler.jobs.(JobAdopter); ok && workflowRunAdoptable(run) && ctx.Err() != nil {
			return logFile.Close()
		}
		scheduler.deferCreateCleanup(ctx, run, workflowName, err)
		return logFile.Close()
	}

	_ = os.Remove(scheduler.createAdmissionPath(run.ID))
	if current.Cancelled() {
		stopCtx, cancelStop := boundedWorkspaceCleanupContext(ctx)
		stopErr := scheduler.shutdownRun(stopCtx, workflowName, run.ID)
		cancelStop()
		if stopErr != nil {

			scheduler.deferCreateCleanup(ctx, current, workflowName, stopErr)
			return nil
		}
	}

	// Carry the persisted identity into terminal eviction evidence checks.
	run.WorkflowName = current.WorkflowName
	return scheduler.observeRun(ctx, run, repository, require, reservation, runObservation{file: logFile, request: &jobRequest})
}

type runObservation struct {
	file          *os.File
	request       *WorkflowRequest
	events        []runprogress.Event
	retained      int64
	adopted       bool
	stopRequested bool
}

// observeRun shares step persistence, publication and issue projection between
// newly submitted and adopted Workflows.
func (scheduler *Scheduler) observeRun(ctx context.Context, run model.Run, repository model.Repository, require mutationRequirement, reservation *submissionOwner, observation runObservation) error {
	logFile := observation.file
	workflowName := run.WorkflowName
	if observation.request != nil {
		workflowName = observation.request.WorkflowName
	}
	journal, journalErr := runlog.OpenPodLogJournal(logFile, scheduler.maxRunLogBytes)
	// A journal is optional delivery evidence. A bad file must not strand an
	// otherwise-owned Workflow or return a fatal error to the dispatcher.
	seekSize, seekErr := logFile.Seek(0, io.SeekEnd)
	if seekErr != nil && journalErr == nil {
		journalErr = seekErr
	}
	logDetached := false
	var logDestination io.Writer = logFile
	retained := observation.retained
	if info, err := logFile.Stat(); err == nil {
		retained = info.Size()
	} else {
		journalErr = errors.Join(journalErr, err)
		if seekErr == nil {
			retained = seekSize
		} else {
			// No safe retained-file access: keep progress/termination observation
			// alive without writing unbounded bytes to a file of unknown size.
			logDetached = true
			logDestination = io.Discard
			log.Printf("WARN: run=%s log continuity lost: retained log unavailable", run.ID)
		}
	}
	observation.retained = retained
	progressLog := newStepProgressLogWriter(newRunLogWriter(logDestination, scheduler.maxRunLogBytes-retained), func(event runprogress.Event) error { return scheduler.logs.AppendStepProgress(run.ID, event) })
	progressLog.logFile = logFile
	progressLog.onContinuityLost = func() {
		if scheduler.auditor == nil {
			return
		}
		auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		_, _ = scheduler.auditor.AppendAuditAction(auditCtx, model.AuditActionSpec{Actor: "oberth", Action: "run.log-continuity.lost", ResourceType: "run", ResourceID: run.ID, Details: `{"reason":"durable-journal-unavailable"}`})
	}
	progressLog.journal = journal
	if journalErr != nil {
		if journal != nil {
			_ = journal.Close()
			progressLog.journal = nil
		}
		progressLog.losePodLogDurability()
		if info, err := logFile.Stat(); err == nil {
			observation.retained = info.Size()
		}
	}
	projectionReady, workflowSettled, journalDeferred := false, false, false
	defer func() {
		if progressLog.journal != nil {
			_ = progressLog.journal.Close()
		}
		scheduler.settlePodJournal(ctx, run.ID, logFile.Name(), projectionReady, workflowSettled, journalDeferred)
	}()

	// The plan is recorded here: after the pipeline object exists, so a run
	// that never reached submission carries no claim about work it would have
	// done, and before the engine's first observation, so the step list is
	// complete from the first poll rather than growing one row per Pod.
	if observation.request != nil {
		if planErr := scheduler.recordStepPlan(ctx, *observation.request); planErr != nil {
			// Plan visibility is an observability record, never a gate. A run
			// whose plan cannot be enumerated still executes, and says so in its
			// own log rather than failing a build over a display concern.
			_, _ = fmt.Fprintf(progressLog, "oberth: step plan unavailable: %v\n", planErr)
		}
	}
	stoppedByOberth := observation.stopRequested
	var jobResult JobResult
	var waitErr error
	if observation.adopted {
		jobResult, waitErr = scheduler.jobs.(JobAdopter).WaitResumed(ctx, workflowName, progressLog, observation.events, observation.retained)
	} else {
		jobResult, waitErr = scheduler.jobs.Wait(ctx, workflowName, progressLog)
	}
	progressLog.recoverPodProjection(scheduler.maxRunLogBytes)
	if progressErr := progressLog.Close(); progressErr != nil && waitErr == nil {
		waitErr = progressErr
	}
	finalizeCtx := ctx
	var cancelFinalize context.CancelFunc
	if ctx.Err() != nil {
		finalizeCtx, cancelFinalize = context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelFinalize()
		if waitErr != nil || !jobResult.Status.Terminal() {
			// A read-only terminal observation may preserve a completed result.
			// Otherwise shutdown releases only the observer, never the Workflow.
			readTerminal := jobResult.Reobserve
			if readTerminal == nil {
				readTerminal = func(ctx context.Context) (JobResult, error) { return scheduler.jobs.TerminalResult(ctx, workflowName) }
			}
			if completed, err := readTerminal(finalizeCtx); err == nil && completed.Status.Terminal() {
				jobResult, waitErr = completed, nil
			} else {
				return logFile.Close()
			}
		}

	}
	// Once at finalization: the WAL already protected every acknowledged
	// batch. Only a synced, indexed projection permits terminal WAL pruning.
	projectionSyncErr := logFile.Sync()
	closeErr := logFile.Close()
	if !logDetached && projectionSyncErr != nil && waitErr == nil {
		waitErr = projectionSyncErr
	}
	if !logDetached && closeErr != nil && waitErr == nil {
		waitErr = closeErr
	}
	index, indexErr := scheduler.logs.BuildIndex(run.ID)
	projectionReady = projectionSyncErr == nil && indexErr == nil
	if !logDetached && indexErr != nil && waitErr == nil {
		waitErr = indexErr
	}
	if waitErr != nil {
		// A transient Wait error (network blip, log reader failure) must not
		// terminalize the run without first checking whether the Workflow
		// actually reached a terminal state. If it did, use the real result
		// rather than fabricating a failure from the observation error.
		terminalRead := jobResult.Reobserve
		if terminalRead == nil {
			terminalRead = func(ctx context.Context) (JobResult, error) { return scheduler.jobs.TerminalResult(ctx, workflowName) }
		}
		if recovered, termErr := terminalRead(finalizeCtx); termErr == nil {
			jobResult = recovered
		} else {
			stopCtx, cancelStop := boundedWorkspaceCleanupContext(finalizeCtx)
			stopErr := scheduler.shutdownRun(stopCtx, workflowName, run.ID)
			cancelStop()
			stoppedByOberth = stoppedByOberth || stopErr == nil
			if recovered, termErr := terminalRead(finalizeCtx); stopErr == nil && termErr == nil {
				jobResult = recovered
				if jobResult.Status == model.RunFailed {
					jobResult.Error = "Oberth requested Argo Stop: " + strings.TrimPrefix(jobResult.Error, "Workflow stopped by its Workflow spec: ")
				}
			} else if _, ok := scheduler.jobs.(JobCreateSettler); ok {
				scheduler.deferCreateCleanup(finalizeCtx, run, workflowName, waitErr, stopErr)
				journalDeferred = true
				return nil
			} else {

				return errors.Join(waitErr, stopErr, fmt.Errorf("workflow %s is awaiting its terminal Argo phase", workflowName))
			}
		}

	}
	// Native terminal observation settles this Workflow.
	workflowSettled = true
	reservation.cancel()
	return scheduler.finishObservedRun(finalizeCtx, run, repository, require, jobResult, index, stoppedByOberth)
}

// recordStepPlan enumerates the run's declared steps from the immutable
// checkout and writes them to the run's durable plan record. An engine that
// cannot enumerate its own pipeline format reports no plan and no error: its
// runs keep the behaviour they have always had.
func (scheduler *Scheduler) recordStepPlan(ctx context.Context, request WorkflowRequest) error {
	planner, ok := scheduler.plannerFor(request)
	if !ok {
		return nil
	}
	planned, err := planner.PlannedSteps(ctx, request)
	if err != nil {
		return err
	}
	if len(planned) == 0 {
		return nil
	}
	return scheduler.logs.WriteStepPlan(request.Run.ID, planned)
}

func (scheduler *Scheduler) plannerFor(request WorkflowRequest) (PipelinePlanner, bool) {
	if request.Run.Release {
		if planner, ok := scheduler.releaseJobs.(PipelinePlanner); ok {
			return planner, true
		}
	}
	planner, ok := scheduler.jobs.(PipelinePlanner)
	return planner, ok
}

// persistSteps writes the run's durable results, keyed by burn and step so a
// step that was planned is updated in place rather than recorded twice.
//
// Ordinals come from the plan, not from this loop's index. Plan order and
// observation order are different orderings -- a DAG declares its tasks in one
// order and runs them in another, and a failed run observes a prefix of the
// second -- and the plan's is the stable one. Renumbering by observation would
// make a run's step list reorder itself as it progressed, and would contradict
// the ordinals the plan record already published for the same steps.
func (scheduler *Scheduler) persistSteps(ctx context.Context, runID string, steps []model.StepResult, index runlog.Index) error {
	ordinals := scheduler.plannedOrdinals(runID)
	next := len(ordinals)
	for observed, result := range steps {
		result.RunID = runID
		if ordinal, planned := ordinals[result.Stage+"\x00"+result.Step]; planned {
			result.Ordinal = ordinal
		} else if len(ordinals) == 0 {
			result.Ordinal = observed
		} else {
			// Observed but never planned: a withItems expansion, or a step
			// whose document Oberth could not enumerate. It sorts after every
			// planned step rather than displacing one.
			result.Ordinal = next
			next++
		}
		result.LogStart, result.LogEnd = logOffsets(index, result.Stage, result.Step)
		if _, err := scheduler.database().PutStepResult(ctx, result); err != nil {
			return fmt.Errorf("persist step result %s/%s: %w", result.Stage, result.Step, err)
		}
	}
	return nil
}

// plannedOrdinals reads the run's plan for its stable ordering. An absent or
// unreadable plan yields an empty map, and persistSteps falls back to the
// observation order it has always used.
func (scheduler *Scheduler) plannedOrdinals(runID string) map[string]int {
	planned, err := scheduler.logs.StepPlan(runID)
	if err != nil {
		return map[string]int{}
	}
	ordinals := make(map[string]int, len(planned))
	for _, step := range planned {
		ordinals[step.Stage+"\x00"+step.Step] = step.Ordinal
	}
	return ordinals
}

func (scheduler *Scheduler) promotionForRun(ctx context.Context, run model.Run) (model.Promotion, error) {
	if run.Trigger != "promotion" {
		return model.Promotion{}, nil
	}
	promotion, err := scheduler.database().PromotionByRun(ctx, run.ID)
	if err != nil {
		return model.Promotion{}, fmt.Errorf("load promotion for merged-tree run: %w", err)
	}
	baseMatches := sameOID(promotion.PreviousSHA, run.BaseSHA) || promotion.PreviousSHA == zeroOID && run.BaseSHA == ""
	if (promotion.Status != model.PromotionPending && !run.Cancelled()) || !sameOID(promotion.ResultSHA, run.TestedSHA) || !baseMatches {
		return promotion, errors.New("service: promotion run does not match its immutable record")
	}
	return promotion, nil
}

// publicationGate reports whether a finalizing run is published upstream:
// only a passed run that is not assisted, after the mutation gate. It
// re-reads the run's durable assisted verdict: a held step continued by the
// agent means the passed tree is not the commit, so the run finishes passed
// in Oberth's own record but is never published. An unreadable verdict is
// returned as an error: finalization stops with the run unfinished, as for
// any other unreadable durable state, instead of finishing a passed run
// silently unpublished.
func (scheduler *Scheduler) publicationGate(ctx context.Context, runID string, result JobResult, require mutationRequirement) (bool, error) {
	if result.Status != model.RunPassed {
		return false, nil
	}
	current, err := scheduler.database().Run(ctx, runID)
	if err != nil {
		return false, fmt.Errorf("read assisted verdict of run %s at finalization: %w", runID, err)
	}
	if current.Assisted || current.Cancelled() {
		return false, nil
	}
	return true, require(ctx)
}

func (scheduler *Scheduler) beginRunPublication(ctx context.Context, repository model.Repository, run model.Run, promotion model.Promotion, result JobResult) (model.Publication, error) {
	_, collectionDeadline := model.SplitArtifactCollectionDeadline(result.Error)
	if promotion.ID != "" {
		return scheduler.database().BeginPublication(ctx, model.PublicationSpec{
			ArtifactCollectionDeadline: collectionDeadline, RepoID: repository.ID, RunID: run.ID, PromotionID: promotion.ID,
			RefKind: model.RefBranch, Ref: promotion.TargetRef,
			PreviousSHA: promotion.PreviousSHA, PreviousKnown: true,
			ResultSHA: promotion.ResultSHA, Actor: promotion.Actor,
		})
	}
	if _, err := canonicalPublicationRef(run.RefKind, run.Ref); err != nil {
		return model.Publication{}, err
	}
	return scheduler.database().BeginPublication(ctx, model.PublicationSpec{
		ArtifactCollectionDeadline: collectionDeadline, RepoID: repository.ID, RunID: run.ID, RefKind: run.RefKind, Ref: run.Ref,
		PreviousKnown: false, ResultSHA: run.SHA, Actor: run.Actor,
	})
}

func (scheduler *Scheduler) recoverPendingPublicationsWithGate(ctx context.Context, require mutationRequirement) error {
	if err := require(ctx); err != nil {
		return err
	}
	pending, err := scheduler.database().PendingPublications(ctx)
	if err != nil {
		return fmt.Errorf("load pending publications: %w", err)
	}
	ids := make([]string, 0, len(pending))
	for _, publication := range pending {
		ids = append(ids, publication.ID)
	}
	if err := scheduler.recoverPublicationIDsWithGate(ctx, ids, require); err != nil {
		return err
	}
	scheduler.retryIssueTrailers(ctx, require)
	return nil
}

func (scheduler *Scheduler) recoverNotifiedPublicationsWithGate(ctx context.Context, require mutationRequirement) error {
	if err := require(ctx); err != nil {
		return err
	}
	if err := scheduler.recoverPublicationIDsWithGate(ctx, scheduler.takePublicationRecoveries(), require); err != nil {
		return err
	}
	scheduler.retryIssueTrailers(ctx, require)
	return nil
}

func (scheduler *Scheduler) recoverPublicationIDsWithGate(ctx context.Context, ids []string, require mutationRequirement) error {
	var recoveryErr error
	for _, id := range ids {
		beatDispatch(ctx)
		finalization, err := scheduler.deliverPublicationWithGate(ctx, id, require)
		if err != nil {
			recoveryErr = errors.Join(recoveryErr, fmt.Errorf("recover publication %s: %w", id, err))
			continue
		}
		if !finalization.Publication.Status.Terminal() {
			continue
		}
		var cleanupErr error
		if finalization.Run.ID != "" {
			beatDispatch(ctx)
			cleanupErr = scheduler.workspaces.cleanupRunDeferred(ctx, finalization.Run.ID)
			beatDispatch(ctx)
		}
		if finalization.Promotion.ID != "" {
			beatDispatch(ctx)
			cleanupErr = errors.Join(cleanupErr, scheduler.workspaces.cleanupPromotionDeferred(ctx, finalization.Promotion.ID))
			beatDispatch(ctx)
		}
		if cleanupErr != nil {
			recoveryErr = errors.Join(recoveryErr, fmt.Errorf("clean finalized publication workspace %s: %w", id, cleanupErr))
		}
	}
	return recoveryErr
}

func (scheduler *Scheduler) DeliverPublication(ctx context.Context, publicationID string) (model.PublicationFinalization, error) {
	return scheduler.deliverPublicationWithGate(ctx, publicationID, scheduler.requireMutation)
}

func (scheduler *Scheduler) deliverPublicationWithGate(
	ctx context.Context,
	publicationID string,
	require mutationRequirement,
) (finalization model.PublicationFinalization, err error) {
	// Deferred before the permit is taken, so it runs after the permit is
	// released: a moved promotion's retry may itself deliver.
	finalizedHere := false
	defer func() {
		if err == nil && finalizedHere {
			scheduler.retryMovedPromotion(ctx, finalization)
		}
	}()
	if strings.TrimSpace(publicationID) == "" {
		return model.PublicationFinalization{}, fmt.Errorf("%w: publication ID is required", ErrInvalidInput)
	}
	// Another delivery can hold the permit across upstream I/O. Waiting for
	// it must not turn that dependency outage into a process restart.
	resume := suspendDispatch(ctx)
	select {
	case <-ctx.Done():
		resume()
		return model.PublicationFinalization{}, ctx.Err()
	case <-scheduler.deliveryPermit:
	}
	resume()
	defer func() { scheduler.deliveryPermit <- struct{}{} }()
	if err := require(ctx); err != nil {
		return model.PublicationFinalization{}, err
	}
	publication, err := scheduler.database().Publication(ctx, publicationID)
	if err != nil {
		return model.PublicationFinalization{}, fmt.Errorf("load publication %s: %w", publicationID, err)
	}
	// Idle backoff ticks neither attempt delivery nor re-evaluate trusted
	// policy. Only actual attempts emit the attempt/outcome evidence pair.
	if publication.Status.Terminal() {
		finalization, err := scheduler.loadPublicationFinalization(ctx, publication)
		if err == nil {
			err = scheduler.finishPublication(ctx, model.Repository{}, finalization)
		}
		return finalization, err
	}
	if time.Now().Before(publication.RetryAfter) && strings.HasPrefix(publication.Error, publicationRetryPrefix) {
		scheduler.queuePublicationRetry(publicationID)
		return scheduler.loadPublicationFinalization(ctx, publication)
	}
	log.Printf("publication attempt run=%s publication=%s status=%s", publication.RunID, publication.ID, publication.Status)
	defer func() {
		outcome := "unconfirmed"
		if err == nil {
			outcome = string(finalization.Publication.Status)
		}
		log.Printf("publication outcome run=%s publication=%s status=%s", publication.RunID, publication.ID, outcome)
	}()
	repository, err := scheduler.database().Repository(ctx, publication.RepoID)
	if err != nil {
		return model.PublicationFinalization{}, fmt.Errorf("load repository for publication %s: %w", publication.ID, err)
	}
	if err := require(ctx); err != nil {
		return model.PublicationFinalization{}, err
	}
	policyErr := scheduler.requirePublicationTestPolicy(ctx, repository, publication)
	if ctx.Err() != nil {
		return finalization, ctx.Err()
	}
	if err := require(ctx); err != nil {
		return model.PublicationFinalization{}, err
	}
	if policyErr != nil {
		var unavailable *workflowpolicy.PolicyRefreshUnavailable
		if errors.As(policyErr, &unavailable) {
			since, err := scheduler.database().SetPublicationPolicyWaiting(ctx, publication.ID, true)
			if err != nil {
				return finalization, err
			}
			if !time.Now().Before(since.Add(scheduler.trustedPolicyWaitLimit())) {
				policyErr = policyWaitFailure(policyErr)
			} else {
				// Policy availability is not a failed build or a push attempt. Keep
				// the durable outbox pending and release the global delivery permit.
				if publication.RunID != "" {
					if _, err := scheduler.setPolicyWaiting(ctx, publication.RunID, true); err != nil {
						return finalization, err
					}
					scheduler.signals.NotifyRun(publication.RunID)
				}
				scheduler.queuePublicationRetry(publication.ID)
				return scheduler.loadPublicationFinalization(ctx, publication)
			}
		}
		finalization, err := scheduler.database().FinalizePublication(ctx, publication.ID, model.PublicationFailed, policyErr.Error())
		if err != nil {
			return model.PublicationFinalization{}, err
		}
		return finalization, scheduler.finishPublication(ctx, repository, finalization)
	}
	if _, err := scheduler.database().SetPublicationPolicyWaiting(ctx, publication.ID, false); err != nil {
		return finalization, err
	}
	if publication.RunID != "" {
		if _, err := scheduler.setPolicyWaiting(ctx, publication.RunID, false); err != nil {
			return finalization, err
		}
	}
	publicationInput := repository.Name
	if upstream, upstreamErr := scheduler.database().Upstream(ctx, repository.UpstreamID); upstreamErr == nil {
		publicationInput = upstream.QualifiedRepo(repository.Name)
	}
	finalization, err = deliverPublication(ctx, scheduler.database(), scheduler.git, databaseAuditor{scheduler.auditor}, repository, publicationInput, publication)
	if err != nil {
		return model.PublicationFinalization{}, err
	}
	finishCtx, cancelFinish := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancelFinish()
	ctx = finishCtx
	if finalization.Publication.Status == model.PublicationPending {
		scheduler.queuePublicationRetry(publicationID)
		finalization, err = scheduler.loadPublicationFinalization(ctx, finalization.Publication)
		if err != nil {
			return finalization, err
		}
	}
	finalizedHere = finalization.Publication.Status.Terminal()
	if err := scheduler.finishPublication(ctx, repository, finalization); err != nil {
		return model.PublicationFinalization{}, err
	}
	return finalization, nil
}

func (scheduler *Scheduler) loadPublicationFinalization(
	ctx context.Context,
	publication model.Publication,
) (model.PublicationFinalization, error) {
	return scheduler.database().PublicationState(ctx, publication.ID)
}

func (scheduler *Scheduler) finishPublication(ctx context.Context, _ model.Repository, finalization model.PublicationFinalization) error {
	if finalization.Promotion.ID != "" {
		scheduler.signals.NotifyPromotion(finalization.Promotion.ID)
	}
	if finalization.Run.ID != "" {
		scheduler.signals.NotifyRun(finalization.Run.ID)
	}
	if finalization.Publication.Status == model.PublicationDelivered {
		scheduler.retryIssueTrailers(ctx, scheduler.requireMutation)
	}
	return nil
}

func (scheduler *Scheduler) finishInfrastructureFailure(ctx context.Context, run model.Run, _ model.Repository, failure error) error {
	defer forgetActiveLogIndex(scheduler.logs, run.ID)
	result := JobResult{Status: model.RunFailed, Phase: "infrastructure", Error: failure.Error()}
	if current, err := scheduler.database().Run(ctx, run.ID); err != nil {
		return err
	} else if current.Cancelled() {
		result = JobResult{Status: model.RunInterrupted, Phase: model.PhaseCancelled, Error: strings.Replace(current.Reason, "cancelling by ", "cancelled by ", 1)}
	}

	var cause interface{ RunFailureCause() string }
	if errors.As(failure, &cause) {
		result.FailureCause = displayFailureText(cause.RunFailureCause())
	}
	var refusal *argoworkflow.AdmissionError
	if errors.As(failure, &refusal) {
		// Deterministic source validation is a document failure, including
		// receipt validation before Create. Keep the complete rule, not the
		// infrastructure wrapper, and use the existing job failure phase.
		result.Phase, result.Error = "job", refusal.Error()
		result.FailureCause = displayFailureText(validationRefusalPrefix + refusal.Error())
	}
	_, err := scheduler.database().FinishRun(ctx, run.ID, model.RunResult{Status: result.Status, Phase: result.Phase, Error: result.Error, FailureCause: result.FailureCause})
	if errors.Is(err, store.ErrInvalidState) {
		scheduler.signals.NotifyRun(run.ID)
		return nil
	}
	if err != nil {
		return errors.Join(failure, err)
	}
	if run.Trigger == "promotion" {
		promotion, promotionErr := scheduler.database().PromotionByRun(ctx, run.ID)
		if promotionErr != nil {
			return errors.Join(failure, promotionErr)
		}
		scheduler.signals.NotifyPromotion(promotion.ID)
	}
	scheduler.signals.NotifyRun(run.ID)
	return nil
}

// Reconcile existing Workflows before deciding that a claimed run is stranded.
// A failed read is not absence and never authorizes restart deletion or execution.
func (scheduler *Scheduler) reconcileStrandedRunsWithGate(ctx context.Context, require mutationRequirement, adopt func(model.Run) error) error {
	if err := require(ctx); err != nil {
		return err
	}
	runs, err := scheduler.database().RunningRunsWithJobs(ctx)
	if err != nil {
		return fmt.Errorf("load stranded runs for reconciliation: %w", err)
	}
	restored, err := loadRestoredRuns(scheduler.restoredRunsFile)
	if err != nil {
		return err
	}
	handled := make(map[string]bool, len(runs))
	for _, run := range runs {
		handled[run.ID] = true
		if run.Cancelled() {
			continue // The durable cancellation pass owns Stop and terminal observation.
		}
		if run.Phase == "preflight" {
			if err := scheduler.failRetiredRun(ctx, run, require); err != nil {
				return err
			}
			continue
		}
		// A restored run caught mid-submission or before its Workflow
		// started is cancelled, never settled and submitted (restored_runs.go).
		if restored[run.ID] && (isPendingCreate(run) || run.Phase == "waiting") {
			if err := scheduler.cancelRestoredRun(ctx, run.ID, require); err != nil {
				return err
			}
			continue
		}
		if adopter, ok := scheduler.jobs.(JobAdopter); ok && workflowRunAdoptable(run) {
			found, readErr := adopter.Adopt(ctx, run)
			if readErr != nil {
				if errors.Is(readErr, argoworkflow.ErrWorkflowCapabilityRequired) {
					readErr = fmt.Errorf("%w: %w", ErrAdoptionRefused, readErr)
				}
				if errors.Is(readErr, ErrAdoptionRefused) {
					if err := scheduler.completeRestartCreateObservation(ctx, run); err != nil {
						return err
					}
				}
				if err := scheduler.handleAdoptionFailure(ctx, adopter, run, readErr, require); err != nil {
					return err
				}
				continue
			}
			if found {
				if err := scheduler.completeRestartCreateObservation(ctx, run); err != nil {
					return err
				}
				if adopt == nil {
					return errors.New("service: adoption requires a worker dispatcher")
				}
				if err := adopt(run); err != nil {
					if failure := scheduler.handleAdoptionFailure(ctx, adopter, run, err, require); failure != nil {
						return failure
					}
				}
				continue
			}
			// A prior POST may still commit. Keep the existing bounded observation
			// window, but recover an existing Workflow instead of deleting it.
			_, contractErr := scheduler.readCreateAdmission(run)
			if isPendingCreate(run) || !errors.Is(contractErr, os.ErrNotExist) {
				scheduler.registerRestartCreate(run)
				continue
			}
			if err := scheduler.failRestartedRun(ctx, run, require); err != nil {
				return err
			}
		} else {
			reconciled, termErr := scheduler.jobs.TerminalResult(ctx, run.WorkflowName)
			if termErr != nil {
				return fmt.Errorf("observe Workflow %s after restart: %w", run.WorkflowName, termErr)
			}
			if err := scheduler.finalizeReconciledRun(ctx, run, reconciled, require); err != nil {
				return err
			}
		}
		cleanupCtx, cancel := boundedWorkspaceCleanupContext(ctx)
		_ = scheduler.workspaces.cleanupRunDeferred(cleanupCtx, run.ID)
		cancel()
	}
	// Restored runs that were queued (or waiting without a Workflow) in the
	// backup may already have run after it; cancel them instead of
	// dispatching them again.
	for id := range restored {
		if handled[id] {
			continue
		}
		if err := scheduler.cancelRestoredRun(ctx, id, require); err != nil {
			return err
		}
	}
	return retireRestoredRunsMarker(scheduler.restoredRunsFile)
}

// finalizeReconciledRun records the durable result of a K8s Job that completed
// while the scheduler was not observing it. Passing runs are routed through
// the publication path so promotion runs finalize correctly.
func (scheduler *Scheduler) finalizeReconciledRun(ctx context.Context, run model.Run, jobResult JobResult, require mutationRequirement) error {
	if err := require(ctx); err != nil {
		return err
	}
	// Build the log index from whatever partial log the reconciled run left
	// behind, so per-step logs remain readable (#437). The log file may not
	// exist if the run never started streaming; an empty index is acceptable.
	index, indexErr := scheduler.logs.BuildIndex(run.ID)
	if indexErr != nil {
		index = runlog.Index{}
	}
	jobResult = scheduler.withResourceFailure(ctx, run.ID, jobResult)
	if err := scheduler.persistSteps(ctx, run.ID, jobResult.Steps, index); err != nil {
		jobResult = JobResult{Status: model.RunFailed, Phase: "collect", Error: err.Error()}
	}
	promotion, promotionErr := scheduler.promotionForRun(ctx, run)
	if promotionErr != nil {
		jobResult = JobResult{Status: model.RunFailed, Phase: "promotion", Error: promotionErr.Error()}
	}
	if publish, gateErr := scheduler.publicationGate(ctx, run.ID, jobResult, require); gateErr != nil {
		return gateErr
	} else if publish {
		repository, err := scheduler.database().Repository(ctx, run.RepoID)
		if err != nil {
			return err
		}
		publication, beginErr := scheduler.beginRunPublication(ctx, repository, run, promotion, jobResult)
		if beginErr != nil {
			jobResult.Status = model.RunFailed
			jobResult.Phase = "publishing"
			jobResult.Error = beginErr.Error()
		} else {
			_, deliveryErr := scheduler.deliverPublicationWithGate(ctx, publication.ID, require)
			if deliveryErr != nil {
				return fmt.Errorf("reconciled publication %s: %w", publication.ID, deliveryErr)
			}
			return nil
		}
	}
	_, err := scheduler.database().FinishRun(ctx, run.ID, model.RunResult{

		TerminalObservation: jobResult.TerminalObservation, Status: jobResult.Status, Phase: jobResult.Phase,
		FailureCause: jobResult.FailureCause, FailedStage: jobResult.FailedStage, FailedStep: jobResult.FailedStep,
		Error: jobResult.Error,
	})
	if errors.Is(err, store.ErrInvalidState) {
		scheduler.signals.NotifyRun(run.ID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("finish reconciled run %s: %w", run.ID, err)
	}
	if promotion.ID != "" {
		scheduler.signals.NotifyPromotion(promotion.ID)
	}
	scheduler.signals.NotifyRun(run.ID)
	return nil
}

func (scheduler *Scheduler) signalQueue() {
	select {
	case scheduler.wake <- struct{}{}:
	default:
	}
}

func (scheduler *Scheduler) takePublicationRecoveries() []string {
	scheduler.publicationMu.Lock()
	defer scheduler.publicationMu.Unlock()
	ids := scheduler.publicationIDs
	scheduler.publicationIDs = nil
	clear(scheduler.publicationSet)
	return ids
}

func (scheduler *Scheduler) requireMutation(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := scheduler.mutationGate(ctx); err != nil {
		return fmt.Errorf("%w: %w", errMutationBlocked, err)
	}
	return nil
}

func (scheduler *Scheduler) awaitMutation(ctx context.Context) error {
	for {
		if err := scheduler.requireMutation(ctx); err == nil {
			return nil
		} else if ctx.Err() != nil {
			return ctx.Err()
		} else if !errors.Is(err, errMutationBlocked) {
			return err
		}
		timer := time.NewTimer(mutationGateRetryInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func queueEmpty(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrNotFound)
}

func validJobRunStatus(status model.RunStatus) bool {
	return status == model.RunPassed || status == model.RunFailed || status == model.RunInterrupted
}

func logOffsets(index runlog.Index, stage, step string) (int64, int64) {
	var start, end int64
	found := false
	for _, value := range index.Ranges {
		if value.Stage != stage || value.Step != step {
			continue
		}
		if !found || value.Start < start {
			start = value.Start
		}
		if !found || value.End > end {
			end = value.End
		}
		found = true
	}
	return start, end
}

func pathWithin(root, path string) bool { return gitoid.PathWithin(root, path) }

func deterministicWorkflowName(repository, runID string) (string, error) {
	if strings.TrimSpace(runID) == "" || len(runID) > 48 || runID[0] == '-' || runID[len(runID)-1] == '-' {
		return "", errors.New("service: run ID cannot form a deterministic Job name")
	}
	for _, character := range runID {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return "", errors.New("service: run ID cannot form a deterministic Job name")
		}
	}
	repositoryFragment := workflowNameFragment(repository)
	if repositoryFragment == "" {
		return "", errors.New("service: repository cannot form a deterministic Job name")
	}
	runPrefix := runID
	if len(runPrefix) > 8 {
		runPrefix = runPrefix[:8]
	}
	digest := sha256.Sum256([]byte(repository + "\x00" + runID))
	suffix := hex.EncodeToString(digest[:6])
	maximumRepositoryBytes := 57 - len("oberth") - len(runPrefix) - len(suffix) - 3
	if len(repositoryFragment) > maximumRepositoryBytes {
		repositoryFragment = strings.TrimRight(repositoryFragment[:maximumRepositoryBytes], "-")
	}
	return "oberth-" + repositoryFragment + "-" + runPrefix + "-" + suffix, nil
}

func workflowNameFragment(value string) string {
	var result strings.Builder
	for _, character := range strings.ToLower(value) {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' {
			result.WriteRune(character)
		} else {
			result.WriteByte('-')
		}
	}
	return strings.Trim(result.String(), "-")
}

// Cleanup is best effort after the durable terminal verdict and index. Active
// owner resumptions retain their checkpoint; cleanup errors never stop dispatch.
func (scheduler *Scheduler) pruneTerminalPodJournal(parent context.Context, runID, logPath string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), time.Second)
	defer cancel()
	run, err := scheduler.store.Run(ctx, runID)
	if err != nil || (!run.Status.Terminal() && !run.Cancelled()) {
		return
	}
	if err := runlog.PrunePodLogJournal(logPath); err != nil {
		log.Print("WARN: terminal pod-log journal cleanup unavailable")
	}
}

func (scheduler *Scheduler) queuePublicationRetry(id string) {
	scheduler.publicationMu.Lock()
	defer scheduler.publicationMu.Unlock()
	if _, exists := scheduler.publicationSet[id]; !exists {
		scheduler.publicationSet[id] = struct{}{}
		scheduler.publicationIDs = append(scheduler.publicationIDs, id)
	}
}
