package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
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
	"github.com/oberthci/oberth/pkg/pipeline"
)

const maximumCIIssueTailBytes = 16 << 10
const mutationGateRetryInterval = time.Second

var errMutationBlocked = errors.New("service: mutation blocked by unavailable audit integrity")

type mutationRequirement func(context.Context) error

type SchedulerConfig struct {
	// DisableInfrastructureAutoRetry turns off the default-on, single automatic retry.
	DisableInfrastructureAutoRetry bool

	// DeclaredAdmission is deprecated and ignored; Kubernetes places step pods.
	DeclaredAdmission DeclaredAdmissionConfig
	Store             SchedulerStore
	Git               WorkspaceGit
	Logs              LogStore
	Jobs              JobController
	ReleaseJobs       ReleaseJobController
	Auditor           Auditor
	Signals           *Signals
	WorkspaceRoot     string
	RemoveWorkspace   func(string) error
	MaxConcurrent     int
	MaxConcurrentFile string
	MaxRunLogBytes    int64
	MutationGate      func(context.Context) error
	// MaxHeadWait is deprecated and ignored; resource drain/backfill is removed.
	MaxHeadWait time.Duration
	// StalledRunTimeout bounds granted runs before submission. Zero is ten minutes.
	StalledRunTimeout time.Duration
	// SizeWeights is deprecated and ignored. Kubernetes places pods by their requests.
	SizeWeights SizeWeights
	// AdmissionDiskFloorBytes is an opt-in free-space admission floor on the data root (#673).
	AdmissionDiskFloorBytes int64
	AdmissionPressure       AdmissionPressureConfig
	// ExclusiveClasses is the administrator's allowlist of server-wide
	// exclusive classes and their concurrent-run limits (#1499). Empty
	// refuses every declaration.
	ExclusiveClasses map[string]int
}

type Scheduler struct {
	disableInfrastructureAutoRetry bool

	databaseRetryCount atomic.Uint64
	dispatchProgress   liveness.Heartbeat
	headProgress       liveness.Heartbeat
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
	maxConcurrent      int // immutable boot default
	capacityFile       string
	capacityOverride   CapacityOverrideReader // server-owned runtime layer (#1497)
	capacityReloadMu   sync.Mutex
	admission          *weightedAdmission
	holdMu             sync.Mutex
	priorityMu         sync.Mutex // serialize durable priority refresh snapshots
	maxRunLogBytes     int64
	stalledRunTimeout  time.Duration
	mutationGate       func(context.Context) error
	wake               chan struct{}
	publicationWake    chan struct{}
	evidenceMu         sync.Mutex
	evidenceExecutions map[string]int
	publicationMu      sync.Mutex
	publicationIDs     []string
	publicationSet     map[string]struct{}
	deliveryPermit     chan struct{}
	capacityWait       func(context.Context) error
	policyWaitTimeout  time.Duration // tests may shorten the fixed production limit
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
	createNow          func() time.Time // test-only reconciliation clock
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
	if config.StalledRunTimeout < 0 {
		return nil, errors.New("service: admission stall timeout must be nonnegative")
	}
	if config.StalledRunTimeout == 0 {
		config.StalledRunTimeout = 10 * time.Minute
	}
	config.AdmissionPressure.HardOnly = true
	if !config.AdmissionPressure.Enabled {
		config.AdmissionPressure.IOFullAvg60Max, config.AdmissionPressure.CPUSomeAvg60Max, config.AdmissionPressure.MaxDefer = 0, 0, 0
	}
	if err := config.AdmissionPressure.Validate(); err != nil {
		return nil, fmt.Errorf("admission pressure: %w", err)
	}
	if config.Store == nil || config.Git == nil || config.Logs == nil || config.Jobs == nil || config.Auditor == nil {
		return nil, errors.New("service: scheduler store, Git, logs, jobs, and auditor are required")
	}
	if config.MaxConcurrent <= 0 {
		config.MaxConcurrent = 3
	}
	if config.MaxConcurrent > 64 {
		return nil, errors.New("service: maximum concurrent runs must be in 1..64")
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
		disableInfrastructureAutoRetry: config.DisableInfrastructureAutoRetry,

		store: config.Store, git: config.Git, logs: config.Logs, jobs: config.Jobs,
		releaseJobs: config.ReleaseJobs,
		auditor:     config.Auditor, signals: signals,
		workspaces: workspaces, workspaceRoot: workspaceRoot, maxConcurrent: config.MaxConcurrent,
		capacityFile:   config.MaxConcurrentFile,
		admission:      newWorkflowAdmissionForJobs(config.MaxConcurrent),
		maxRunLogBytes: config.MaxRunLogBytes, mutationGate: mutationGate, stalledRunTimeout: config.StalledRunTimeout,
		wake: make(chan struct{}, 1), publicationWake: make(chan struct{}, 1),
		createWake: make(chan struct{}, 1), createPending: make(map[string]*pendingCreate),
		publicationSet: make(map[string]struct{}), deliveryPermit: deliveryPermit, capacityWait: waitCapacityRetry,
	}
	scheduler.workspaces.owners = scheduler.database()
	// Group/VM parked claims release their dispatch slots. The admission gate
	// independently enforces the administrator's workflow ceiling. Kubernetes
	// decides whether each step pod fits its node by the declared requests.
	scheduler.admission.maxRuns = config.MaxConcurrent
	scheduler.admission.capacityConfig = model.AdmissionCapacityConfig{MaxConcurrentJobs: config.MaxConcurrent, BootDefault: config.MaxConcurrent, Source: "boot", File: config.MaxConcurrentFile}
	scheduler.admission.pressure = newPressureCheck(config.AdmissionPressure)
	scheduler.admission.exclusiveLimits = maps.Clone(config.ExclusiveClasses)
	if config.AdmissionDiskFloorBytes > 0 {
		scheduler.admission.diskFloor = newDiskFloorCheck(workspaceRoot, config.AdmissionDiskFloorBytes)
	}
	scheduler.admission.onPark = scheduler.signalQueue
	scheduler.admission.onGrant = scheduler.signalQueue
	if state, ok := config.Store.(admissionHoldStore); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		hold, err := state.AdmissionHold(ctx)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("load admission hold: %w", err)
		}
		scheduler.admission.adminHold = hold
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
	event := receiveSpec(request.EventID, request.Actor, request.Repository.ID, model.RefBranch, request.Branch,
		request.OldSHA, request.SHA, request.SHA, "queued")
	spec := model.RunSpec{
		PriorityRequest: request.PriorityRequest,
		Assist:          request.Assist,
		Reason:          request.Reason,
		RepoID:          request.Repository.ID, RefKind: model.RefBranch, Ref: request.Branch,
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
	if err == nil && result.Enqueued.DeferredReason != "" {
		scheduler.signalQueue()
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
		PriorityRequest: request.PriorityRequest,
		RepoID:          request.Repository.ID, RefKind: model.RefTag, Ref: request.Tag,
		SHA: request.ObjectSHA, Actor: request.Actor, Release: true, Credentialed: true, Trigger: "tag", TestedSHA: request.CommitSHA,
	})
	if err != nil {
		return model.EnqueueRunResult{}, err
	}
	if err := scheduler.AcceptEnqueue(ctx, enqueued); err != nil {
		return enqueued, err
	}
	return enqueued, nil
}

func (scheduler *Scheduler) AcceptEnqueue(ctx context.Context, enqueued model.EnqueueRunResult) error {
	if !enqueued.Duplicate {
		if err := scheduler.completeCancellations(ctx, enqueued.Cancellations); err != nil {
			scheduler.signalQueue()
			return err
		}
	}
	scheduler.signalQueue()
	return nil
}

// AcceptCancellations executes the obligations an explicit run cancel
// recorded: a waiting run's admission reservation is withdrawn at once and a
// claimed run's Workflow is deleted (an absent one is fine). A failed deletion
// stays pending for the cancellation pass, exactly as for a supersede.
//
// AI-CONTRACT: jobs.Delete runs synchronously on the caller's context, which
// for run_cancel is the MCP request. A client disconnect or request timeout
// cancels that context, so the deletion fails and the obligation stays
// pending; the cancel itself is already committed, and the scheduler's next
// cancellation pass (queue wake or restart) deletes the Workflow.
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
	// an admission reservation but owns no Workflow. Withdraw it first, and
	// regardless of the audit gate: it is in-memory scheduling state only,
	// and leaving it in place keeps the run at the head of the FIFO (or ahead
	// of its group) until a restart.
	scheduler.abandonAdmissions(ctx, cancellations)
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
		// Same-ID restart recovery clears the run's current Workflow name.
		// The cancellation ledger still owns the prior Workflow until deletion
		// completes. An empty name can still belong to a live checkout.
		if cancellation.WorkflowName == "" {
			// The live owner may still be checking out. Only worker join or
			// owner restart can confirm absence and release this workspace.
			continue
		}
		if isPendingCreate(run) || scheduler.ownsCreateCleanup(run.ID) {
			scheduler.registerCreateCleanup(run, nil, isPendingCreate(run))
			continue // Never wait for Kubernetes on dispatch/push/cancel.
		}
		if err := runDispatchExternal(ctx, func(ctx context.Context) error {
			return scheduler.jobs.Delete(ctx, cancellation.WorkflowName, cancellation.RunID)
		}); err != nil {
			return fmt.Errorf("delete superseded Job %s: %w", cancellation.WorkflowName, &pendingJobDeletionError{err})
		}
		if err := scheduler.database().CompleteObservedRunCancellation(ctx, cancellation); err != nil {
			return fmt.Errorf("complete cancellation for run %s: %w", cancellation.RunID, err)
		}
		scheduler.admission.releaseDeletedRun(cancellation.RunID)
		cleanupCtx, cancelCleanup := boundedWorkspaceCleanupContext(ctx)
		cleanupErr := scheduler.workspaces.cleanupRunDeferred(cleanupCtx, cancellation.RunID)
		cancelCleanup()
		if cleanupErr != nil {
			return fmt.Errorf("clean canceled run workspace %s: %w", cancellation.RunID, cleanupErr)
		}
	}
	return nil
}

// abandonAdmissions withdraws the ungranted admission reservations of runs a
// cancellation obligation has already terminalized. Only a terminal durable
// record releases a reservation here. Preflight admission releases immediately;
// a submitted main Workflow keeps its reservation until deletion is accepted.
func (scheduler *Scheduler) abandonAdmissions(ctx context.Context, cancellations []model.RunCancellation) {
	for _, cancellation := range cancellations {
		run, err := scheduler.database().Run(ctx, cancellation.RunID)
		if err != nil || !run.Status.Terminal() {
			continue
		}
		scheduler.admission.abandon(run.QueueSequence)
	}
}

// Recovered reports that durable owner recovery and Workflow adoption have
// completed. It is safe to use from readiness probes without store access.
func (scheduler *Scheduler) Recovered() bool { return scheduler.startupRecovered.Load() }

// Run owns oldest-eligible claims for one server process and fills at most
// MaxConcurrent slots. A pending publication temporarily gates only its exact
// repository/ref; queue changes and completions wake the scheduler through
// bounded channels. A claimed run parked on its concurrency group waits for
// another run of its own repository rather than for the node, so it frees its
// slot for the claims behind it (see dispatchSlotAvailable).
func (scheduler *Scheduler) Run(ctx context.Context) error {
	workerCtx, cancelWorkers := context.WithCancel(ctx)
	workerCtx = context.WithValue(workerCtx, databaseRetryKey{}, &databaseRetryScope{lifetime: workerCtx, owner: scheduler})
	defer cancelWorkers()
	scheduler.refreshRuntimeCapacity(workerCtx)
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
	if err := scheduler.reconcileStrandedRunsWithGate(workerCtx, scheduler.awaitMutation, func(run model.Run, size pipeline.Size) error {
		var err error
		var pods *argoworkflow.PodDemand
		if reader, ok := scheduler.jobs.(interface {
			AdoptedPodDemand(model.Run) (*argoworkflow.PodDemand, error)
		}); ok {
			pods, err = reader.AdoptedPodDemand(run)
			if err != nil {
				return err
			}
		}
		reservation, err := scheduler.admission.adopt(run, size, pods)
		if err != nil {
			return err
		}
		if reader, ok := scheduler.jobs.(interface {
			AdoptedExclusiveClasses(model.Run) ([]string, error)
		}); ok {
			classes, e := reader.AdoptedExclusiveClasses(run)
			if e != nil {
				return e
			}
			scheduler.admission.adoptExclusive(reservation, classes)
		}
		if reader, ok := scheduler.jobs.(interface {
			AdoptedResourceFallback(model.Run) (model.ResourceAdmissionFallback, error)
		}); ok {
			fallback, e := reader.AdoptedResourceFallback(run)
			if e != nil {
				return e
			}
			scheduler.admission.mu.Lock()
			reservation.fallback = fallback
			scheduler.admission.mu.Unlock()
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
	if err := scheduler.recoverInterruptedPromotions(workerCtx); err != nil {
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
	if scheduler.capacityFile != "" {
		workers.Add(1)
		go func() { defer workers.Done(); scheduler.watchRuntimeCapacity(workerCtx, nil) }()
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		scheduler.runHeadEvents(workerCtx)
	}()
	workers.Add(1)
	go func() { defer workers.Done(); scheduler.runCreateCleanup(workerCtx) }()
	completed := make(chan error, max(2*scheduler.maxConcurrent, len(adopted)))
	active := len(adopted)
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
	priorityPoll := time.NewTicker(priorityPollInterval)
	defer priorityPoll.Stop()
	ticks := priorityPoll.C
	if scheduler.dispatchTicks != nil {
		ticks = scheduler.dispatchTicks
	}
	for {
		scheduler.dispatchProgress.Beat()
		if err := scheduler.drainDeferredBranches(dispatchCtx, scheduler.awaitDispatchMutation); err != nil {
			join()
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		priorityErr := scheduler.refreshPriorities(dispatchCtx)
		if priorityErr != nil && !store.IsTransientLockError(priorityErr) {
			join()
			if ctx.Err() != nil {
				return nil
			}
			return priorityErr
		}
		for priorityErr == nil {
			scheduler.dispatchProgress.Beat()
			if !scheduler.dispatchSlotAvailable(active) {
				available, err := scheduler.priorityDispatchAvailable(dispatchCtx, active)
				if store.IsTransientLockError(err) {
					break // Keep background tasks alive; retry on the next notification or poll.
				}
				if err != nil {
					join()
					if ctx.Err() != nil {
						return nil
					}
					return err
				}
				if !available {
					break
				}
			}
			if err := scheduler.awaitDispatchMutation(dispatchCtx); err != nil {
				join()
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			if err := scheduler.awaitDispatchAdmission(dispatchCtx); err != nil {
				join()
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			run, err := scheduler.database().ClaimNextRun(dispatchCtx)
			if queueEmpty(err) {
				break
			}
			if err != nil {
				join()
				if ctx.Err() != nil || errors.Is(err, context.Canceled) {
					return nil
				}
				return fmt.Errorf("claim priority run: %w", err)
			}
			reservation := scheduler.admission.reserveRun(run)
			active++
			workers.Add(1)
			go func(run model.Run, reservation *admissionReservation) {
				defer workers.Done()
				err := scheduler.executeAndCleanupReservedWithGate(workerCtx, run, scheduler.awaitMutation, reservation)
				select {
				case completed <- err:
				case <-workerCtx.Done():
				}
			}(run, reservation)
		}
		// Claim higher work before letting a refreshed/aged waiter grant.
		scheduler.admission.mu.Lock()
		scheduler.admission.grantLocked()
		scheduler.admission.mu.Unlock()
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
			if err := scheduler.completePendingCancellationsWithGate(dispatchCtx, scheduler.awaitDispatchMutation); err != nil {
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
		case err := <-completed:
			active--
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

// dispatchSlotAvailable reports whether the dispatch loop may claim another
// run while active claimed runs are in flight.
//
// A run parked on VM capacity (#278) or its concurrency group (#658) waits for another run of its
// own repository, not for node resources, and holds no admission weight. If
// it also held a dispatch slot, one repository with a few queued runs in one
// group could fill every slot and stall unrelated repositories: exactly the
// whole-host reservation the group exists to replace. Parked runs therefore
// do not count, up to MaxConcurrent of them, which bounds the claimed runs (and
// their checkouts) at twice the ceiling. The gate mirrors MaxConcurrent as its
// run ceiling, so admitted runs never exceed it when a parked run's group
// frees up. Without parked runs this is exactly `active < MaxConcurrent`.
//
// Past that ceiling the loop claims only into spare weight (#1021): when every
// claimed run already holds its grant and the budget still has room, one more
// run is claimed, so S-sized runs fill the 2 x MaxConcurrent weight budget
// instead of stopping at MaxConcurrent runs. At most one such claim waits at a
// time, so this never piles up checkouts behind a full budget.
func (scheduler *Scheduler) dispatchSlotAvailable(active int) bool {
	// Held claims cannot be replenished by ClaimNextRun. Exclude all of them
	// so a new hold cannot strand its allowed classes behind earlier claims.
	limit := scheduler.concurrentJobs()
	held, other := scheduler.admission.dispatchParked()
	active = max(0, active-held)
	parked := min(other, limit)
	if active-parked < limit {
		return true
	}
	return scheduler.admission.spareWeightForClaim(active - parked)
}

// ProcessNext is a deterministic single-run entrypoint for recovery tools and
// focused tests. The long-running server normally uses Run.
func (scheduler *Scheduler) ProcessNext(ctx context.Context) error {
	if err := scheduler.requireMutation(ctx); err != nil {
		return err
	}
	if err := scheduler.drainDeferredBranches(ctx, scheduler.requireMutation); err != nil {
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
	reservation := scheduler.admission.reserveRun(run)
	return scheduler.executeAndCleanupReservedWithGate(ctx, run, require, reservation)
}

func (scheduler *Scheduler) executeAndCleanupReservedWithGate(
	ctx context.Context,
	run model.Run,
	require mutationRequirement,
	reservation *admissionReservation,
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
	if workflowRunAdoptable(run) {
		current, err := scheduler.database().Run(cleanupCtx, run.ID)
		if err != nil {
			return errors.Join(executeErr, err)
		}
		if current.Status == model.RunRunning && current.WorkflowName != "" {
			return executeErr
		}
	}
	settleErr := scheduler.database().CompleteSupersededRunCancellationWithoutJob(cleanupCtx, run.ID)
	cleanupErr := scheduler.workspaces.cleanupRunDeferred(cleanupCtx, run.ID)
	if run.Trigger == "promotion" {
		promotion, err := scheduler.database().PromotionByRun(cleanupCtx, run.ID)
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("load promotion workspace owner for run %s: %w", run.ID, err))
		} else {
			cleanupErr = errors.Join(cleanupErr, scheduler.workspaces.cleanupPromotionDeferred(cleanupCtx, promotion.ID))
		}
	}
	return errors.Join(executeErr, settleErr, cleanupErr)
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

func (scheduler *Scheduler) execute(ctx context.Context, run model.Run, require mutationRequirement, reservation *admissionReservation) error {
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
	restarted := run.Reason == store.RestartRequeueReason || strings.HasPrefix(run.Reason, store.RestartRequeueReason+"; ")
	if restarted {
		// ClaimNextRun fences this owner behind the old Workflow's confirmed
		// cleanup. Recreate only this run's scratch directory; retained logs
		// and publication evidence live outside it.
		path, pathErr := runWorkspacePath(scheduler.workspaceRoot, run.ID)
		if pathErr != nil {
			return scheduler.finishInfrastructureFailure(ctx, run, repository, pathErr)
		}
		if err := scheduler.workspaces.remove(path); err != nil {
			return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("prepare restarted run workspace: %w", err))
		}
		if err := os.Remove(scheduler.createAdmissionPath(run.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return scheduler.finishInfrastructureFailure(ctx, run, repository, err)
		}
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
	if restarted && errors.Is(err, os.ErrExist) {
		logFile, _, _, err = scheduler.logs.Resume(run.ID)
	}
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
	if current.Preempted() {
		_ = logFile.Close()
		if err := scheduler.completePendingCancellationsWithGate(ctx, require); err != nil {
			return err
		}
		scheduler.signals.NotifyRun(run.ID)
		return nil
	}
	jobRequest := WorkflowRequest{WorkflowName: workflowName, Run: run, Repository: repository, SourceDir: sourceDir, UpstreamName: upstream.Name, UpstreamOrg: upstream.Org()}
	size := pipeline.M // Historical wire metadata; it has no admission effect.
	if !scheduler.admission.workflowOnly {
		size, err = scheduler.pipelineSize(ctx, jobRequest)
		if err != nil {
			_ = logFile.Close()
			return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("read pipeline resource size: %w", err))
		}
	}
	// Priority is advisory: an unavailable grant read withholds the boost,
	// never the release's execution or publication.
	priorityMaximum, priorityErr := scheduler.releasePriorityMaximum(ctx, run)
	jobRequest.Size = size
	group, groupLimit, err := scheduler.pipelineConcurrencyGroup(ctx, jobRequest)
	if err != nil {
		_ = logFile.Close()
		return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("read pipeline concurrency group: %w", err))
	}
	// Persist the YAML declaration, including removal, before any grant.
	if groupErr := scheduler.database().SetRunConcurrencyGroup(ctx, run.ID, group, groupLimit); groupErr != nil {
		_ = logFile.Close()
		return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("record concurrency group: %w", groupErr))
	}
	run.ConcurrencyGroup, run.ConcurrencyLimit = group, groupLimit
	jobRequest.Run.ConcurrencyGroup, jobRequest.Run.ConcurrencyLimit = group, groupLimit
	jobRequest.ConcurrencyGroup = group
	receipt, err := scheduler.pipelineDemandReceipt(ctx, jobRequest)
	if err != nil {
		_ = logFile.Close()
		return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("read pipeline source receipt: %w", err))
	}
	jobRequest.DemandReceipt = receipt
	// Classes come from the same fragment-resolved graph Build submits.
	exclusive, err := scheduler.pipelineExclusiveClasses(ctx, jobRequest, receipt)
	if err != nil {
		_ = logFile.Close()
		return scheduler.finishInfrastructureFailure(ctx, run, repository, err)
	}
	jobRequest.Exclusive = exclusive
	var pods *argoworkflow.PodDemand
	var fallback model.ResourceAdmissionFallback
	{
		pods, fallback, err = scheduler.admission.selectDeclaredDemand(receipt)
		if err != nil {
			_ = logFile.Close()
			return scheduler.finishInfrastructureFailure(ctx, run, repository, err)
		}
	}
	if !scheduler.admission.workflowOnly && pods == nil {
		if err := scheduler.authorizeRunSize(ctx, run, repository, size); err != nil {
			_ = logFile.Close()
			return scheduler.finishInfrastructureFailure(ctx, run, repository, err)
		}
	}
	jobRequest.PodDemand = pods
	jobRequest.ResourceFallback = fallback
	scheduler.admissionRequested(ctx, run.ID)
	var requests *argoworkflow.PodRequests
	if receipt != nil {
		demand, _ := receipt.WorkflowPodDemand()
		requests = queuePodRequests(demand)
	}
	headSince, estimate := scheduler.admissionHistory(ctx, run)
	releaseAdmission, err := reservation.acquireRequest(ctx, admissionRequest{
		RunID: run.ID, RepoID: run.RepoID, Repo: repository.Name, Ref: run.Ref, Size: size, Group: group, GroupLimit: groupLimit, Exclusive: exclusive, Pods: pods, Requests: requests, Fallback: fallback,
		RefKind: run.RefKind, SHA: run.SHA, HeadSince: headSince, Estimate: estimate, PriorityMaximum: priorityMaximum, PriorityLookupFailed: priorityErr != nil,
	})
	if errors.Is(err, errAdmissionAbandoned) {
		// Superseded or cancelled while waiting: the reservation is already
		// gone, so settle the cancellation like the pre-admission check does.
		_ = logFile.Close()
		if err := scheduler.completePendingCancellationsWithGate(ctx, require); err != nil {
			return err
		}
		scheduler.signals.NotifyRun(run.ID)
		return nil
	}
	if err != nil {
		_ = logFile.Close()
		var conflict *concurrencyGroupConflict
		if errors.As(err, &conflict) {
			return scheduler.finishInfrastructureFailure(ctx, run, repository, err)
		}
		return err
	}
	defer releaseAdmission()
	// Keep submission and recovery bound to the contract the gate actually held.
	scheduler.admission.mu.Lock()
	actualPods := reservation.pods
	if actualPods != nil {
		copy := *actualPods
		actualPods = &copy
	}
	fallback = reservation.fallback
	scheduler.admission.mu.Unlock()
	if pods != nil && actualPods == nil {
		_ = logFile.Close()
		return scheduler.finishInfrastructureFailure(ctx, run, repository, errors.New("declared resource reservation lost before submission"))
	}
	pods = actualPods
	jobRequest.PodDemand = pods
	jobRequest.ResourceFallback = fallback
	if reservation.wasDelayed() {
		current, err = scheduler.database().Run(ctx, run.ID)
		if err != nil {
			_ = logFile.Close()
			return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("verify run before weighted admission: %w", err))
		}
		if current.Preempted() {
			_ = logFile.Close()
			if err := scheduler.completePendingCancellationsWithGate(ctx, require); err != nil {
				return err
			}
			scheduler.signals.NotifyRun(run.ID)
			return nil
		}
	}
	if err := require(ctx); err != nil {
		_ = logFile.Close()
		return err
	}
	admissionCtx, stopAdmission := context.WithTimeout(ctx, scheduler.stalledRunTimeout)
	err = scheduler.awaitAdmission(admissionCtx, jobRequest)
	stopAdmission()
	if err != nil {
		_ = logFile.Close()
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("controller profile verification unavailable for %s before workflow submission", scheduler.stalledRunTimeout))
		}
		return err
	}
	current, err = scheduler.database().MarkRunAdmitted(ctx, run.ID)
	if err != nil {
		_ = logFile.Close()
		return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("record weighted admission: %w", err))
	}
	if current.Preempted() {
		_ = logFile.Close()
		if err := scheduler.completePendingCancellationsWithGate(ctx, require); err != nil {
			return err
		}
		scheduler.signals.NotifyRun(run.ID)
		return nil
	}
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
	if !run.Credentialed && scheduler.pipelineCredentialed(ctx, jobRequest) {
		if credErr := scheduler.database().SetRunCredentialed(ctx, run.ID); credErr != nil {
			_ = logFile.Close()
			return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("mark run credentialed: %w", credErr))
		}
		run.Credentialed = true
	}
	if err = require(ctx); err != nil {
		_ = logFile.Close()
		return err
	}
	if err != nil {
		_ = logFile.Close()
		return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("persist deterministic Job intent: %w", err))
	}
	if current.Status != model.RunRunning {
		_ = logFile.Close()
		return nil
	}
	// A run granted without waiting was last read before admission. Re-read
	// it so a supersede or cancel that landed since stops here, before any
	// Workflow exists, instead of after MarkRunSubmitted.
	if fresh, err := scheduler.database().Run(ctx, run.ID); err != nil {
		_ = logFile.Close()
		return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("verify run before Job creation: %w", err))
	} else if fresh.Preempted() {
		_ = logFile.Close()
		if err := scheduler.completePendingCancellationsWithGate(ctx, require); err != nil {
			return err
		}
		scheduler.signals.NotifyRun(run.ID)
		return nil
	}

	if _, ok := scheduler.jobs.(JobCreateSettler); ok {
		if err := scheduler.recordResourceAdmission(run, workflowName, size, pods, fallback); err != nil {
			_ = logFile.Close()
			return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("record Workflow admission contract: %w", err))
		}
	}

	switch {
	case run.Release:
		if scheduler.releaseJobs == nil {
			_ = logFile.Close()
			return scheduler.finishInfrastructureFailure(ctx, run, repository, errors.New("release Job capability is unavailable"))
		}
		err = scheduler.releaseJobs.CreateRelease(ctx, jobRequest)
	default:
		err = scheduler.jobs.CreateWorkflow(ctx, jobRequest)
	}
	if err != nil {
		var uncertain interface{ AmbiguousCreate() bool }
		if errors.As(err, &uncertain) && uncertain.AmbiguousCreate() {
			_ = logFile.Close()
			scheduler.deferCreateCleanup(ctx, run, workflowName, reservation, err)
			return nil // The reconciler owns this grant, not the dispatcher.
		}
		if _, ok := scheduler.jobs.(JobAdopter); ok && workflowRunAdoptable(run) && ctx.Err() != nil {
			reservation.retain()
			return logFile.Close()
		}
		_ = logFile.Close()
		// Definite rejection never deletes a foreign name. Ambiguous POSTs stay
		// owned by the settlement task above until absence is confirmed.
		return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("create Job: %w", err))
	}
	reservation.markSubmitted()
	current, err = scheduler.database().MarkRunSubmitted(ctx, run.ID)
	if err != nil {
		if _, ok := scheduler.jobs.(JobAdopter); ok && workflowRunAdoptable(run) && ctx.Err() != nil {
			reservation.retain()
			return logFile.Close()
		}
		failure := errors.Join(fmt.Errorf("verify run after Job creation: %w", err), logFile.Close())
		cleanupCtx, cancelCleanup := boundedWorkspaceCleanupContext(ctx)
		deleteErr := scheduler.jobs.Delete(cleanupCtx, workflowName, run.ID)
		cancelCleanup()
		if deleteErr != nil {
			if _, ok := scheduler.jobs.(JobCreateSettler); ok {
				scheduler.deferCreateCleanup(ctx, run, workflowName, reservation)
				return nil
			}
			// Keep the run active with its durable Job name. Returning the error
			// stops this owner; startup recovery will then create and retry the
			// exact cancellation instead of terminalizing an orphaned Job. The
			// admission stays held too: the Workflow may still be running.
			reservation.retain()
			return errors.Join(failure, fmt.Errorf("delete Job %s after run verification failure: %w", workflowName, deleteErr))
		}
		return scheduler.finishInfrastructureFailure(ctx, run, repository, failure)
	}
	_ = os.Remove(scheduler.createAdmissionPath(run.ID))
	if current.Preempted() {
		_ = logFile.Close()
		cleanupCtx, cancelCleanup := boundedWorkspaceCleanupContext(ctx)
		if err := scheduler.jobs.Delete(cleanupCtx, workflowName, run.ID); err != nil {
			// A cancellation pass may already have settled the obligation
			// before this Workflow existed; make it pending again so the next
			// pass or restart deletes it rather than orphaning it.
			rearmErr := scheduler.database().RearmRunCancellation(context.WithoutCancel(ctx), run.ID, workflowName)
			cancelCleanup()
			reservation.retain()
			return errors.Join(fmt.Errorf("delete superseded Job %s: %w", workflowName, err), rearmErr)
		}
		completeErr := scheduler.database().CompleteRunCancellation(cleanupCtx, run.ID, workflowName)
		cancelCleanup()
		scheduler.signals.NotifyRun(run.ID)
		return completeErr
	}

	// Carry the persisted identity into terminal eviction evidence checks.
	run.WorkflowName = current.WorkflowName
	return scheduler.observeRun(ctx, run, repository, require, reservation, runObservation{file: logFile, request: &jobRequest})
}

type runObservation struct {
	file     *os.File
	request  *WorkflowRequest
	events   []runprogress.Event
	retained int64
	adopted  bool
}

// observeRun shares step persistence, publication and issue projection between
// newly submitted and adopted Workflows.
func (scheduler *Scheduler) observeRun(ctx context.Context, run model.Run, repository model.Repository, require mutationRequirement, reservation *admissionReservation, observation runObservation) error {
	logFile := observation.file
	workflowName := run.WorkflowName
	if observation.request != nil {
		workflowName = observation.request.WorkflowName
	}
	checkoutSHA := run.TestedSHA
	if checkoutSHA == "" {
		checkoutSHA = run.SHA
	}
	releaseAdmission := reservation.release
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
	projectionReady, workflowSettled := false, false
	defer func() {
		if progressLog.journal != nil {
			_ = progressLog.journal.Close()
		}
		if projectionReady && workflowSettled {
			scheduler.pruneTerminalPodJournal(ctx, run.ID, logFile.Name())
		}
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
		if _, ok := scheduler.jobs.(JobAdopter); ok && workflowRunAdoptable(run) {
			if waitErr != nil || !jobResult.Status.Terminal() {
				// The next owner resumes this exact Workflow. A terminal
				// observation already returned by Wait can finalize here.
				reservation.retain()
				return logFile.Close()
			}
		} else {
			// Check whether the Job already reached a terminal state before the
			// scheduler stopped. A completed Job must record its real result, not
			// a fabricated "interrupted."
			if reconciled, termErr := scheduler.jobs.TerminalResult(finalizeCtx, workflowName); termErr == nil {
				jobResult = reconciled
				waitErr = nil
			} else {
				// Job not terminal, absent, or state unreadable at shutdown.
				// Reset under the same ID when eligible: the durable
				// obligation owns the Job deletion, and whichever process next
				// runs a cancellation pass — the successor's startup in the
				// upgrade case — executes it before the run is claimed again.
				if requeued, requeueErr := scheduler.database().RequeueStrandedRun(finalizeCtx, run.ID); requeueErr == nil {
					_ = logFile.Close()
					scheduler.signals.NotifyRun(run.ID)
					scheduler.signals.NotifyRun(requeued.ID)
					return nil
				} else if !errors.Is(requeueErr, store.ErrRequeueIneligible) {
					_ = logFile.Close()
					return fmt.Errorf("requeue run %s during scheduler shutdown: %w", run.ID, requeueErr)
				}
				// Ineligible work keeps the conservative contract: delete, then
				// terminalize as interrupted.
				if deleteErr := scheduler.jobs.Delete(finalizeCtx, workflowName, run.ID); deleteErr != nil {
					_ = logFile.Close()
					reservation.retain()
					return fmt.Errorf("cancel Job %s during scheduler shutdown: %w", workflowName, deleteErr)
				}
				jobResult = JobResult{Status: model.RunInterrupted, Phase: "interrupted", Error: "scheduler stopped"}
				waitErr = nil
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
	failurePhase := "job"
	var infrastructure InfrastructureError
	if errors.As(waitErr, &infrastructure) && infrastructure.InfrastructureFailure() {
		failurePhase = "infrastructure"
	}
	if observation.adopted && waitErr != nil {
		// Cancel and supersede delete the Workflow under the adopted Wait;
		// their durable obligation, not this observation error, settles it.
		if current, lookupErr := scheduler.database().Run(finalizeCtx, run.ID); lookupErr == nil && current.Preempted() {
			if err := scheduler.completePendingCancellationsWithGate(finalizeCtx, require); err != nil {
				reservation.retain()
				return err
			}
			workflowSettled = true
			scheduler.signals.NotifyRun(run.ID)
			return nil
		}
		// Otherwise it settles exactly like a submitted run: a terminal
		// Workflow keeps its real result, and one that can no longer be
		// observed is deleted and fails only this run, never the scheduler.
		if infrastructure == nil || infrastructure.InfrastructureFailure() {
			failurePhase = "infrastructure"
		}
		waitErr = fmt.Errorf("observe adopted Workflow %s: %w", workflowName, waitErr)
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
			waitErr = nil
		} else {
			// Invariant: a run must not be terminalized as failed while the
			// Workflow is STILL RUNNING (possibly with credentials). When
			// TerminalResult does not yield a terminal result, require a
			// successful ownership-checked Delete before recording failure.
			// If Delete fails, the Workflow may still be alive — return an
			// error that leaves the run owned and reconcilable by the
			// restart pass rather than recording it failed while the
			// workload runs untracked.
			deleteCtx, cancelDelete := boundedWorkspaceCleanupContext(finalizeCtx)
			deleteErr := scheduler.jobs.Delete(deleteCtx, workflowName, run.ID)
			cancelDelete()
			if deleteErr != nil {
				var expired interface{ AdmissionExpiryReason() string }
				var unsettled interface{ AmbiguousCreate() bool }
				if errors.As(waitErr, &expired) || (errors.As(deleteErr, &unsettled) && unsettled.AmbiguousCreate()) {
					if _, ok := scheduler.jobs.(JobCreateSettler); ok {
						scheduler.deferCreateCleanup(finalizeCtx, run, workflowName, reservation, waitErr)
						return nil
					}
				}
				// The admission stays held with the run: the next member of
				// its concurrency group must not start beside a Workflow that
				// may still be alive.
				reservation.retain()
				return fmt.Errorf("delete Workflow %s before terminalizing failed run %s: %w",
					workflowName, run.ID, deleteErr)
			}
			// An observation error cannot erase a positively identified failed step.
			jobResult.Status, jobResult.Phase, jobResult.Error = model.RunFailed, failurePhase, waitErr.Error()
		}
	}
	// The Workflow is now terminal or its deletion has been accepted, so its
	// weight and, for a grouped run, its concurrency group (#658) pass to the
	// next run here, before the slow finalization and publication below.
	// Releasing as soon as Wait returned handed the group on while a Workflow
	// whose Wait merely failed could still be running.
	workflowSettled = true
	releaseAdmission()
	if !validJobRunStatus(jobResult.Status) {
		jobResult.Status, jobResult.Phase, jobResult.Error = model.RunFailed, "job", "Job returned an invalid terminal status"
	}
	if gateErr := require(finalizeCtx); gateErr != nil {
		return gateErr
	}
	jobResult = scheduler.withResourceFailure(finalizeCtx, run.ID, jobResult)
	if err := scheduler.persistSteps(finalizeCtx, run.ID, jobResult.Steps, index); err != nil {
		jobResult = JobResult{Status: model.RunFailed, Phase: "collect", Error: err.Error()}
	}

	promotion, promotionErr := scheduler.promotionForRun(finalizeCtx, run)
	if promotionErr != nil {
		jobResult = JobResult{Status: model.RunFailed, Phase: "promotion", Error: promotionErr.Error()}
	}
	if jobResult.Status == model.RunPassed && !scheduler.runAssisted(finalizeCtx, run.ID) {
		if gateErr := require(finalizeCtx); gateErr != nil {
			return gateErr
		}
		publication, beginErr := scheduler.beginRunPublication(finalizeCtx, repository, run, promotion)
		if beginErr != nil {
			jobResult.Status = model.RunFailed
			jobResult.Phase = "publishing"
			jobResult.Error = beginErr.Error()
		} else {
			_, deliveryErr := scheduler.deliverPublicationWithGate(finalizeCtx, publication.ID, require)
			if deliveryErr != nil {
				return fmt.Errorf("publication %s: %w", publication.ID, deliveryErr)
			}
			return nil
		}
	}
	failureTail := ""
	// Interrupted work does not project a CI-red issue. Historical-log loss
	// must not replace its already-established cut verdict with a red collect error.
	if jobResult.Status == model.RunFailed {
		tail, tailErr := scheduler.failureExcerpt(run.ID, jobResult.FailedStage, jobResult.FailedStep)
		if tailErr != nil {
			jobResult.Status = model.RunFailed
			jobResult.Phase = "collect"
			collectionErr := fmt.Errorf("read bounded failure tail: %w", tailErr)
			if jobResult.Error != "" {
				collectionErr = errors.Join(errors.New(jobResult.Error), collectionErr)
			}
			jobResult.Error = collectionErr.Error()
		} else {
			failureTail = string(tail)
		}
	}
	finished, err := scheduler.database().FinishRun(finalizeCtx, run.ID, model.RunResult{
		AutoRetryInfrastructure: scheduler.infrastructureAutoRetry(run, jobResult),
		TerminalObservation:     jobResult.TerminalObservation, Status: jobResult.Status, Phase: jobResult.Phase, TestedSHA: checkoutSHA, BaseSHA: jobResult.BaseSHA,
		FailureCause: jobResult.FailureCause, FailedStage: jobResult.FailedStage, FailedStep: jobResult.FailedStep, Error: jobResult.Error,
		FailureTail: failureTail,
	})
	if errors.Is(err, store.ErrInvalidState) {
		// A newer branch push may have superseded the run while its Job was
		// terminating. The superseded record is already authoritative.
		scheduler.signals.NotifyRun(run.ID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("finish run %s: %w", run.ID, err)
	}
	if finished.AutoRetriedBy != "" {
		scheduler.signals.NotifyRun(finished.AutoRetriedBy)
		scheduler.signalQueue()
	}
	if promotion.ID != "" {
		scheduler.signals.NotifyPromotion(promotion.ID)
	}
	scheduler.signals.NotifyRun(run.ID)
	return nil
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
		if result.DeclaredSize == "" {
			result.DeclaredSize = pipeline.M
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
	if promotion.Status != model.PromotionPending || !sameOID(promotion.ResultSHA, run.TestedSHA) || !baseMatches {
		return promotion, errors.New("service: promotion run does not match its immutable record")
	}
	return promotion, nil
}

// runAssisted re-reads the run's durable assisted verdict at finalization: a
// held step continued by the agent means the passed tree is not the commit,
// so the run finishes passed in Oberth's own record but is never published
// upstream. An unreadable run is treated as assisted, failing closed.
func (scheduler *Scheduler) runAssisted(ctx context.Context, runID string) bool {
	current, err := scheduler.database().Run(ctx, runID)
	if err != nil {
		log.Printf("WARN: run=%s assisted verdict unreadable at finalization; publication skipped: %v", runID, err)
		return true
	}
	return current.Assisted
}

func (scheduler *Scheduler) beginRunPublication(ctx context.Context, repository model.Repository, run model.Run, promotion model.Promotion) (model.Publication, error) {
	if promotion.ID != "" {
		return scheduler.database().BeginPublication(ctx, model.PublicationSpec{
			RepoID: repository.ID, RunID: run.ID, PromotionID: promotion.ID,
			RefKind: model.RefBranch, Ref: promotion.TargetRef,
			PreviousSHA: promotion.PreviousSHA, PreviousKnown: true,
			ResultSHA: promotion.ResultSHA, Actor: promotion.Actor,
		})
	}
	if _, err := canonicalPublicationRef(run.RefKind, run.Ref); err != nil {
		return model.Publication{}, err
	}
	return scheduler.database().BeginPublication(ctx, model.PublicationSpec{
		RepoID: repository.ID, RunID: run.ID, RefKind: run.RefKind, Ref: run.Ref,
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
	var cause interface{ RunFailureCause() string }
	if errors.As(failure, &cause) {
		result.FailureCause = displayFailureText(cause.RunFailureCause())
	}
	var admission *argoworkflow.AdmissionError
	var ceiling *argoworkflow.ResourceCeilingError
	retry := !errors.As(failure, &admission) && !errors.As(failure, &ceiling) && scheduler.infrastructureAutoRetry(run, result)
	finished, err := scheduler.database().FinishRun(ctx, run.ID, model.RunResult{AutoRetryInfrastructure: retry, Status: model.RunFailed, Phase: "infrastructure", Error: failure.Error(), FailureCause: result.FailureCause})
	if errors.Is(err, store.ErrInvalidState) {
		scheduler.signals.NotifyRun(run.ID)
		return nil
	}
	if err != nil {
		return errors.Join(failure, err)
	}
	if finished.AutoRetriedBy != "" {
		scheduler.signals.NotifyRun(finished.AutoRetriedBy)
		scheduler.signalQueue()
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

// reconcileStrandedRunsWithGate queries K8s for runs that were in-flight when
// the previous scheduler process stopped. Runs whose Jobs completed get their
// real durable result; eligible Workflows resume under the same run.
// Absent Workflows and other run kinds keep their existing fallback. This
// must run before completePendingCancellationsWithGate,
// which would delete the K8s Jobs before their terminal state can be read.
func (scheduler *Scheduler) reconcileStrandedRunsWithGate(ctx context.Context, require mutationRequirement, adopt func(model.Run, pipeline.Size) error) error {
	if err := require(ctx); err != nil {
		return err
	}
	runs, err := scheduler.database().RunningRunsWithJobs(ctx)
	if err != nil {
		return fmt.Errorf("load stranded runs for reconciliation: %w", err)
	}
	for _, run := range runs {
		if run.Phase == "preflight" {
			if err := scheduler.failRetiredRun(ctx, run, require); err != nil {
				return err
			}
			continue
		}
		if isPendingCreate(run) {
			scheduler.registerCreateCleanup(run, nil, true)
			continue
		}
		if adopter, ok := scheduler.jobs.(JobAdopter); ok && workflowRunAdoptable(run) {
			{
				size, found, err := adopter.Adopt(ctx, run)
				if errors.Is(err, argoworkflow.ErrWorkflowCapabilityRequired) {
					if err := scheduler.failRetiredRun(ctx, run, require); err != nil {
						return err
					}
					continue
				}
				if err != nil {
					return adoptionFailure(adopter, run, err)
				}
				if found {
					if adopt == nil {
						return errors.New("service: adoption requires a worker dispatcher")
					}
					if err := adopt(run, size); err != nil {
						return adoptionFailure(adopter, run, err)
					}
					continue
				}
			}
		}
		if _, canSettle := scheduler.jobs.(JobCreateSettler); canSettle && run.Phase == "waiting" {
			{
				_, contractErr := scheduler.readCreateAdmission(run)
				// File absence is the pre-feature/never-submitted path. An
				// unreadable or corrupt file still denotes a possible POST;
				// registration conservatively restores that unknown contract.
				if !errors.Is(contractErr, os.ErrNotExist) {
					scheduler.registerCreateCleanup(run, nil, false)
					continue
				}
			}
		}
		reconciled, termErr := scheduler.jobs.TerminalResult(ctx, run.WorkflowName)
		if termErr == nil {
			if err := scheduler.finalizeReconciledRun(ctx, run, reconciled, require); err != nil {
				return err
			}
		} else {
			requeued, requeueErr := scheduler.database().RequeueStrandedRun(ctx, run.ID)
			if requeueErr == nil {
				// The same run stays queued behind its prior Workflow's durable
				// cleanup obligation. Do not complete that obligation here;
				// the cancellation pass must observe deletion first.
				scheduler.signals.NotifyRun(run.ID)
				scheduler.signals.NotifyRun(requeued.ID)
				cleanupCtx, cancelCleanup := boundedWorkspaceCleanupContext(ctx)
				_ = scheduler.workspaces.cleanupRunDeferred(cleanupCtx, run.ID)
				cancelCleanup()
				continue
			}
			if !errors.Is(requeueErr, store.ErrRequeueIneligible) {
				return fmt.Errorf("requeue stranded run %s: %w", run.ID, requeueErr)
			}
			deleteCtx, cancelDelete := boundedWorkspaceCleanupContext(ctx)
			deleteErr := scheduler.jobs.Delete(deleteCtx, run.WorkflowName, run.ID)
			cancelDelete()
			if deleteErr != nil {
				// Deletion failed — the Workflow may still be running with
				// credentials. Leave the run owned for the next restart pass
				// rather than recording interrupted while the workload lives.
				return fmt.Errorf("delete stranded Workflow %s for run %s: %w", run.WorkflowName, run.ID, deleteErr)
			}
			errMsg := "job not in terminal state at restart; result unrecoverable"
			if !errors.Is(termErr, ErrJobNotTerminal) {
				errMsg = fmt.Sprintf("job state unreadable at restart: %v", termErr)
			}
			if gateErr := require(ctx); gateErr != nil {
				return gateErr
			}
			_, finishErr := scheduler.database().FinishRun(ctx, run.ID, model.RunResult{
				Status: model.RunInterrupted, Phase: "interrupted", Error: errMsg,
			})
			if finishErr != nil && !errors.Is(finishErr, store.ErrInvalidState) {
				return fmt.Errorf("interrupt stranded run %s: %w", run.ID, finishErr)
			}
			if run.Trigger == "promotion" {
				if promotion, promotionErr := scheduler.database().PromotionByRun(ctx, run.ID); promotionErr == nil {
					scheduler.signals.NotifyPromotion(promotion.ID)
				}
			}
			scheduler.signals.NotifyRun(run.ID)
		}
		_ = scheduler.database().CompleteRunCancellation(ctx, run.ID, run.WorkflowName)
		cleanupCtx, cancelCleanup := boundedWorkspaceCleanupContext(ctx)
		_ = scheduler.workspaces.cleanupRunDeferred(cleanupCtx, run.ID)
		if run.Trigger == "promotion" {
			if promotion, promotionErr := scheduler.database().PromotionByRun(cleanupCtx, run.ID); promotionErr == nil {
				_ = scheduler.workspaces.cleanupPromotionDeferred(cleanupCtx, promotion.ID)
			}
		}
		cancelCleanup()
	}
	return nil
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
	if jobResult.Status == model.RunPassed && !scheduler.runAssisted(ctx, run.ID) {
		repository, err := scheduler.database().Repository(ctx, run.RepoID)
		if err != nil {
			return err
		}
		if gateErr := require(ctx); gateErr != nil {
			return gateErr
		}
		publication, beginErr := scheduler.beginRunPublication(ctx, repository, run, promotion)
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
	finished, err := scheduler.database().FinishRun(ctx, run.ID, model.RunResult{
		AutoRetryInfrastructure: scheduler.infrastructureAutoRetry(run, jobResult),
		TerminalObservation:     jobResult.TerminalObservation, Status: jobResult.Status, Phase: jobResult.Phase,
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
	if finished.AutoRetriedBy != "" {
		scheduler.signals.NotifyRun(finished.AutoRetriedBy)
		scheduler.signalQueue()
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

// RunSizeLimiter reports the largest admission size a repository's runs may
// be admitted at (#1021): L unless an administrator granted XL.
type RunSizeLimiter interface {
	MaxRunSize(context.Context, int64) (pipeline.Size, error)
}

// authorizeRunSize refuses a branch, promotion or schedule run whose resolved
// size exceeds its repository's size grant, before it takes any weight: one
// repository's document declaring XL would otherwise make every other
// repository wait for a full drain. A tag run keeps its reviewed
// release.yaml size. A store without the capability (test doubles) limits
// nothing; the production store always has it.
func (scheduler *Scheduler) authorizeRunSize(ctx context.Context, run model.Run, repository model.Repository, size pipeline.Size) error {
	limiter, ok := scheduler.store.(RunSizeLimiter)
	if run.Release || !ok {
		return nil
	}
	maximum, err := retryDatabase(ctx, "MaxRunSize", func(ctx context.Context) (pipeline.Size, error) { return limiter.MaxRunSize(ctx, run.RepoID) })
	if err != nil {
		return fmt.Errorf("read repository size grant: %w", err)
	}
	if sizeRank(size) <= sizeRank(maximum) {
		return nil
	}
	return fmt.Errorf("pipeline declares size %s for this %s run, but repository %s may run at most %s; "+
		"an administrator raises the maximum with `oberth size grant %s --max %s` (MCP size_grant), "+
		"or the pipeline declares a smaller size (oberth.ci/size-by-trigger can keep %s for promotion and schedule runs only)",
		size.Effective(), runTriggerName(run), repository.Name, maximum, repository.Name, size.Effective(), size.Effective())
}

func runTriggerName(run model.Run) string {
	switch run.Trigger {
	case "promotion", "schedule":
		return run.Trigger
	default:
		return "branch"
	}
}

func (scheduler *Scheduler) pipelineSize(ctx context.Context, request WorkflowRequest) (pipeline.Size, error) {
	if request.Run.Release {
		if sizer, ok := scheduler.releaseJobs.(PipelineSizer); ok {
			return sizer.PipelineSize(ctx, request)
		}
	}
	if sizer, ok := scheduler.jobs.(PipelineSizer); ok {
		return sizer.PipelineSize(ctx, request)
	}
	return pipeline.M, nil
}

// pipelineConcurrencyGroup reads the concurrency group the run's own pipeline
// document declares, selecting the engine exactly as pipelineSize does (the
// engine derives the document, build.yaml or release.yaml, from the durable
// run). An engine without the capability runs every pipeline ungrouped.
func (scheduler *Scheduler) pipelineConcurrencyGroup(ctx context.Context, request WorkflowRequest) (string, int, error) {
	read := func(engine any) (string, int, error, bool) {
		if reader, ok := engine.(PipelineConcurrencyContractReader); ok {
			g, n, e := reader.PipelineConcurrencyContract(ctx, request)
			return g, n, e, true
		}
		if reader, ok := engine.(PipelineConcurrencyGrouper); ok {
			g, e := reader.PipelineConcurrencyGroup(ctx, request)
			return g, 1, e, true
		}
		return "", 1, nil, false
	}
	if request.Run.Release {
		if g, n, e, ok := read(scheduler.releaseJobs); ok {
			return g, n, e
		}
	}
	g, n, e, _ := read(scheduler.jobs)
	return g, n, e
}

// PipelineExclusiveClassReader reads the exclusive classes the immutable
// checked-out pipeline's steps declare (#1499), by the same static read.
type PipelineExclusiveClassReader interface {
	PipelineExclusiveClasses(context.Context, WorkflowRequest) ([]string, error)
}

// pipelineExclusiveClasses reads the run's declared exclusive classes and
// refuses any class outside the administrator allowlist. The admitted source
// receipt is authoritative: it holds the fragment-resolved graph, so a class
// declared only inside a fragment is admitted, limited and refused exactly
// like one in the repository's own document. Build refuses any later
// difference. The refusal is an admission error: a rerun cannot change it,
// so it is never auto-retried.
func (scheduler *Scheduler) pipelineExclusiveClasses(ctx context.Context, request WorkflowRequest, receipt *argoworkflow.DemandReceipt) ([]string, error) {
	var classes []string
	var err error
	if receipt != nil {
		classes, err = receipt.ExclusiveClasses()
	} else {
		reader, ok := any(scheduler.jobs).(PipelineExclusiveClassReader)
		if release, releaseOK := any(scheduler.releaseJobs).(PipelineExclusiveClassReader); request.Run.Release && releaseOK {
			reader, ok = release, true
		}
		if !ok {
			return nil, nil
		}
		classes, err = reader.PipelineExclusiveClasses(ctx, request)
	}
	if err != nil {
		return nil, &argoworkflow.AdmissionError{Cause: fmt.Errorf("read pipeline exclusive classes: %w", err)}
	}
	if !validExclusiveClasses(classes) {
		return nil, &argoworkflow.AdmissionError{Cause: errors.New("read pipeline exclusive classes: invalid declaration")}
	}
	for _, class := range classes {
		if scheduler.admission.exclusiveLimits[class] < 1 {
			return nil, &argoworkflow.AdmissionError{Cause: fmt.Errorf("exclusive class %q is not on this server's allowlist (admission.exclusiveClasses); ask the administrator to configure it or remove the %s annotation", class, argoworkflow.ExclusiveAnnotation)}
		}
	}
	return classes, nil
}

// pipelineCredentialed reports whether the run's pipeline declares secret-store
// paths that make it a credentialed run. Releases are always credentialed
// (already set at enqueue time); this method detects CI pipelines with declared
// secrets so the run can be marked before the log writer is created.
//
// Design: on detector error the result is false (not credentialed). This is
// fail-open for scheduling (the run proceeds as a non-credentialed branch
// build, which has zero secrets and zero publication rights) and fail-safe
// for records (a credentialed run that is not marked will be re-detected at
// admission time). The false result never grants access it should not.
func (scheduler *Scheduler) pipelineCredentialed(ctx context.Context, request WorkflowRequest) bool {
	if request.Run.Release || request.Run.Credentialed {
		return true
	}
	if detector, ok := scheduler.jobs.(PipelineCredentialDetector); ok {
		credentialed, err := detector.PipelineCredentialed(ctx, request)
		if err != nil {
			log.Printf("WARN: pipelineCredentialed decode error for run %s: %v (treating as non-credentialed)", request.Run.ID, err)
			return false
		}
		return credentialed
	}
	return false
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

func (scheduler *Scheduler) awaitAdmission(ctx context.Context, requests ...WorkflowRequest) error {
	if gate, ok := scheduler.jobs.(interface {
		AwaitRunAdmission(context.Context, WorkflowRequest) error
	}); ok && len(requests) == 1 {
		return gate.AwaitRunAdmission(ctx, requests[0])
	}
	if gate, ok := scheduler.jobs.(interface{ AwaitAdmission(context.Context) error }); ok {
		return gate.AwaitAdmission(ctx)
	}
	return ctx.Err()
}

// Cleanup is best effort after the durable terminal verdict and index. Active
// owner resumptions retain their checkpoint; cleanup errors never stop dispatch.
func (scheduler *Scheduler) pruneTerminalPodJournal(parent context.Context, runID, logPath string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), time.Second)
	defer cancel()
	run, err := scheduler.store.Run(ctx, runID)
	if err != nil || !run.Status.Terminal() {
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

func (scheduler *Scheduler) recoverInterruptedPromotions(ctx context.Context) error {
	hook := scheduler.promotionRetrier.Load()
	if hook == nil || hook.retrier == nil {
		return nil
	}
	recovery, ok := hook.retrier.(interface{ RecoverInterruptedPromotions(context.Context) error })
	if !ok {
		return nil
	}
	_, err := retryDatabase(ctx, "RecoverInterruptedPromotions", func(ctx context.Context) (struct{}, error) {
		return struct{}{}, recovery.RecoverInterruptedPromotions(ctx)
	})
	return err
}
