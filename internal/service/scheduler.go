package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"log"

	"github.com/oberthci/oberth/internal/gitoid"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/vmrunner"
	"github.com/oberthci/oberth/pkg/periapsis"
)

const maximumCIIssueTailBytes = 16 << 10
const mutationGateRetryInterval = time.Second

var errMutationBlocked = errors.New("service: mutation blocked by unavailable audit integrity")

type mutationRequirement func(context.Context) error

type SchedulerConfig struct {
	Store            SchedulerStore
	Git              WorkspaceGit
	Logs             LogStore
	Jobs             JobController
	ReleaseJobs      ReleaseJobController
	TrustedPreflight TrustedPreflight
	Auditor          Auditor
	Signals          *Signals
	WorkspaceRoot    string
	RemoveWorkspace  func(string) error
	MaxConcurrent    int
	MaxRunLogBytes   int64
	MutationGate     func(context.Context) error
	// MaxHeadWait bounds bounded backfill (#1085): while the queue head that
	// does not fit has waited less than this, later runs that fit are
	// admitted beside it. Zero is strict FIFO; serve defaults it to
	// DefaultMaxHeadWait.
	MaxHeadWait time.Duration
	// SizeWeights is the operator's admission weight per size (#1021); the
	// zero value selects DefaultSizeWeights.
	SizeWeights SizeWeights
	// AdmissionDiskFloorBytes is an opt-in disk-space admission floor (#673).
	// When positive, admission is held until the filesystem containing
	// WorkspaceRoot has at least this many free bytes. Default 0 = disabled.
	AdmissionDiskFloorBytes int64
}

type Scheduler struct {
	store           SchedulerStore
	git             WorkspaceGit
	logs            LogStore
	jobs            JobController
	releaseJobs     ReleaseJobController
	trusted         TrustedPreflight
	auditor         Auditor
	signals         *Signals
	workspaces      *workspaceLifecycle
	workspaceRoot   string
	maxConcurrent   int
	admission       *weightedAdmission
	maxRunLogBytes  int64
	mutationGate    func(context.Context) error
	wake            chan struct{}
	publicationWake chan struct{}
	publicationMu   sync.Mutex
	publicationIDs  []string
	publicationSet  map[string]struct{}
	deliveryPermit  chan struct{}
	capacityWait    func(context.Context) error
	// promotionRetrier re-promotes a promotion whose delivery found its
	// target moved (#1097); nil leaves such a promotion failed.
	promotionRetrier atomic.Pointer[promotionRetrierHook]
	// onStartupRecovered, when set (tests only), is called once Run has
	// finished startup recovery and is about to enter its dispatch loop.
	onStartupRecovered func()
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
	hook.retrier.RetryMovedPromotion(ctx, finalization.Promotion)
}

func NewScheduler(config SchedulerConfig) (*Scheduler, error) {
	if config.Store == nil || config.Git == nil || config.Logs == nil || config.Jobs == nil || config.Auditor == nil {
		return nil, errors.New("service: scheduler store, Git, logs, jobs, and auditor are required")
	}
	if config.MaxConcurrent <= 0 {
		config.MaxConcurrent = 3
	}
	if config.SizeWeights == (SizeWeights{}) {
		config.SizeWeights = DefaultSizeWeights
	}
	if err := config.SizeWeights.Validate(); err != nil {
		return nil, fmt.Errorf("service: admission size weights: %w", err)
	}
	if config.MaxRunLogBytes <= 0 {
		config.MaxRunLogBytes = 64 << 20
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
		releaseJobs: config.ReleaseJobs, trusted: config.TrustedPreflight,
		auditor: config.Auditor, signals: signals,
		workspaces: workspaces, workspaceRoot: workspaceRoot, maxConcurrent: config.MaxConcurrent,
		admission:      newWeightedAdmissionForJobsWeights(config.MaxConcurrent, config.SizeWeights),
		maxRunLogBytes: config.MaxRunLogBytes, mutationGate: mutationGate,
		wake: make(chan struct{}, 1), publicationWake: make(chan struct{}, 1),
		publicationSet: make(map[string]struct{}), deliveryPermit: deliveryPermit, capacityWait: waitTrustedCapacity,
	}
	// Runs parked on their concurrency group (#658) do not hold a dispatch
	// slot, so the gate carries its own run ceiling, and a newly parked run
	// wakes the dispatch loop to claim past it. The ceiling is the weight
	// budget (#1021): M runs still fill it at maxConcurrentJobs, while S runs
	// (weight 1) pack two per M slot. A grant wakes the loop too, so it can
	// claim into weight a small run left spare.
	scheduler.admission.maxRuns = scheduler.admission.capacity
	scheduler.admission.onPark = scheduler.signalQueue
	scheduler.admission.onGrant = scheduler.signalQueue
	if config.MaxHeadWait > 0 {
		scheduler.admission.maxHeadWait = config.MaxHeadWait
	}
	if _, ok := config.Store.(HeadWaitStore); ok {
		scheduler.admission.headEvents = make(chan headEvent, 256)
	}
	// Opt-in disk floor (#673): when positive, admission holds until the
	// filesystem has enough free bytes.
	if config.AdmissionDiskFloorBytes > 0 {
		scheduler.admission.diskFloor = newDiskFloorCheck(
			workspaceRoot, config.AdmissionDiskFloorBytes,
		)
	}
	return scheduler, nil
}

func (scheduler *Scheduler) EnqueueCI(ctx context.Context, request CIRequest) (model.EnqueueRunResult, error) {
	if err := scheduler.requireMutation(ctx); err != nil {
		return model.EnqueueRunResult{}, err
	}
	if strings.TrimSpace(request.EventID) == "" || request.Repository.ID <= 0 || strings.TrimSpace(request.Branch) == "" ||
		!validOID(request.SHA) || strings.TrimSpace(request.Actor) == "" {
		return model.EnqueueRunResult{}, fmt.Errorf("%w: CI request is invalid", ErrInvalidInput)
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
	enqueued, err := scheduler.store.EnqueueReceiveEvent(ctx, event, model.RunSpec{
		RepoID: request.Repository.ID, RefKind: model.RefBranch, Ref: request.Branch,
		SHA: request.SHA, Actor: request.Actor, Trigger: trigger, TestedSHA: testedSHA, BaseSHA: request.BaseSHA,
	})
	if err != nil {
		return model.EnqueueRunResult{}, err
	}
	if err := scheduler.AcceptEnqueue(ctx, enqueued); err != nil {
		return enqueued, err
	}
	return enqueued, nil
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
	enqueued, err := scheduler.store.EnqueueReceiveEvent(ctx, event, model.RunSpec{
		RepoID: request.Repository.ID, RefKind: model.RefTag, Ref: request.Tag,
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

// completePendingCancellations finishes durable supersession and owner-restart
// cleanup only after the idempotent Job deletion succeeds.
func (scheduler *Scheduler) completePendingCancellations(ctx context.Context) error {
	return scheduler.completePendingCancellationsWithGate(ctx, scheduler.requireMutation)
}

func (scheduler *Scheduler) completePendingCancellationsWithGate(ctx context.Context, require mutationRequirement) error {
	pending, err := scheduler.store.PendingRunCancellations(ctx)
	if err != nil {
		return fmt.Errorf("load pending run cancellations: %w", err)
	}
	return scheduler.completeCancellationsWithGate(ctx, pending, require)
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
	if err := require(ctx); err != nil {
		return err
	}
	for _, cancellation := range cancellations {
		if strings.TrimSpace(cancellation.JobName) == "" {
			continue
		}
		if err := scheduler.jobs.Delete(ctx, cancellation.JobName, cancellation.RunID); err != nil {
			return fmt.Errorf("delete superseded Job %s: %w", cancellation.JobName, err)
		}
		if err := scheduler.store.CompleteRunCancellation(ctx, cancellation.RunID, cancellation.JobName); err != nil {
			return fmt.Errorf("complete cancellation for run %s: %w", cancellation.RunID, err)
		}
		cleanupCtx, cancelCleanup := boundedWorkspaceCleanupContext(ctx)
		cleanupErr := scheduler.workspaces.cleanupRun(cleanupCtx, cancellation.RunID)
		cancelCleanup()
		if cleanupErr != nil {
			return fmt.Errorf("clean canceled run workspace %s: %w", cancellation.RunID, cleanupErr)
		}
	}
	return nil
}

// abandonAdmissions withdraws the ungranted admission reservations of runs a
// cancellation obligation has already terminalized. Only a terminal durable
// record releases a reservation here; a granted one stays with its owner,
// which releases it once the Workflow is terminal or its deletion was
// accepted.
func (scheduler *Scheduler) abandonAdmissions(ctx context.Context, cancellations []model.RunCancellation) {
	for _, cancellation := range cancellations {
		run, err := scheduler.store.Run(ctx, cancellation.RunID)
		if err != nil || !run.Status.Terminal() {
			continue
		}
		scheduler.admission.abandon(run.QueueSequence)
	}
}

// Run owns oldest-eligible claims for one server process and fills at most
// MaxConcurrent slots. A pending publication temporarily gates only its exact
// repository/ref; queue changes and completions wake the scheduler through
// bounded channels. A claimed run parked on its concurrency group waits for
// another run of its own repository rather than for the node, so it frees its
// slot for the claims behind it (see dispatchSlotAvailable).
func (scheduler *Scheduler) Run(ctx context.Context) error {
	workerCtx, cancelWorkers := context.WithCancel(ctx)
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
	if err := scheduler.recoverTrustedPreflights(workerCtx); err != nil {
		return err
	}
	if err := scheduler.reconcileStrandedRunsWithGate(workerCtx, scheduler.awaitMutation); err != nil {
		return err
	}
	if err := scheduler.completePendingCancellationsWithGate(workerCtx, scheduler.awaitMutation); err != nil {
		return err
	}
	if err := scheduler.recoverPendingPublicationsWithGate(workerCtx, scheduler.awaitMutation); err != nil {
		return err
	}
	if err := scheduler.recoverOwnedWorkspaces(workerCtx); err != nil {
		return err
	}
	if scheduler.onStartupRecovered != nil {
		scheduler.onStartupRecovered()
	}
	scheduler.signalQueue()
	workers.Add(1)
	go func() {
		defer workers.Done()
		scheduler.runHeadEvents(workerCtx)
	}()
	completed := make(chan error, 2*scheduler.maxConcurrent)
	active := 0
	for {
		for scheduler.dispatchSlotAvailable(active) {
			if err := scheduler.awaitMutation(workerCtx); err != nil {
				join()
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			run, err := scheduler.store.ClaimNextRun(workerCtx)
			if queueEmpty(err) {
				break
			}
			if err != nil {
				join()
				if ctx.Err() != nil || errors.Is(err, context.Canceled) {
					return nil
				}
				return fmt.Errorf("claim FIFO run: %w", err)
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
		select {
		case <-ctx.Done():
			join()
			return nil
		case <-scheduler.wake:
			if err := scheduler.completePendingCancellationsWithGate(workerCtx, scheduler.awaitMutation); err != nil {
				join()
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		case <-scheduler.publicationWake:
			if err := scheduler.recoverNotifiedPublicationsWithGate(workerCtx, scheduler.awaitMutation); err != nil {
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
	parked := min(scheduler.admission.groupParked(), scheduler.maxConcurrent)
	if active-parked < scheduler.maxConcurrent {
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
	run, err := scheduler.store.ClaimNextRun(ctx)
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
	executeErr := scheduler.execute(ctx, run, require, reservation)
	if errors.Is(executeErr, errMutationBlocked) {
		return executeErr
	}
	cleanupCtx, cancelCleanup := boundedWorkspaceCleanupContext(ctx)
	defer cancelCleanup()
	settleErr := scheduler.store.CompleteSupersededRunCancellationWithoutJob(cleanupCtx, run.ID)
	cleanupErr := scheduler.workspaces.cleanupRun(cleanupCtx, run.ID)
	if run.Trigger == "promotion" {
		promotion, err := scheduler.store.PromotionByRun(cleanupCtx, run.ID)
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("load promotion workspace owner for run %s: %w", run.ID, err))
		} else {
			cleanupErr = errors.Join(cleanupErr, scheduler.workspaces.cleanupPromotion(cleanupCtx, promotion.ID))
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
	repository, err := scheduler.store.Repository(ctx, run.RepoID)
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
	upstream, err := scheduler.store.Upstream(ctx, repository.UpstreamID)
	if err != nil {
		return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("load run repository upstream: %w", err))
	}
	if err := scheduler.git.Checkout(ctx, upstream.QualifiedRepo(repository.Name), checkoutSHA, sourceDir); err != nil {
		return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("checkout run workspace: %w", err))
	}
	requirement, err := scheduler.trustedRequirement(ctx, repository, checkoutSHA)
	if err != nil {
		return scheduler.finishInfrastructureFailure(ctx, run, repository, err)
	}
	logFile, err := scheduler.logs.Create(run.ID)
	if err != nil {
		return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("create run log: %w", err))
	}
	jobName, err := deterministicJobName(repository.Name, run.ID)
	if err != nil {
		_ = logFile.Close()
		return scheduler.finishInfrastructureFailure(ctx, run, repository, err)
	}
	var current model.Run
	if requirement.Contract == nil {
		current, err = scheduler.store.SetRunJobName(ctx, run.ID, jobName)
	} else {
		current, err = scheduler.store.Run(ctx, run.ID)
	}
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
	jobRequest := JobRequest{JobName: jobName, Run: run, Repository: repository, SourceDir: sourceDir}
	size, err := scheduler.pipelineSize(ctx, jobRequest)
	if err != nil {
		_ = logFile.Close()
		return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("read pipeline resource size: %w", err))
	}
	if requirement.Contract != nil {
		if sized, ok := scheduler.trusted.(interface {
			Size(context.Context, JobRequest) (periapsis.Size, error)
		}); ok {
			preflightSize, sizeErr := sized.Size(ctx, jobRequest)
			if sizeErr != nil {
				_ = logFile.Close()
				return scheduler.finishInfrastructureFailure(ctx, run, repository, sizeErr)
			}
			if sizeRank(preflightSize) > sizeRank(size) {
				size = preflightSize
			}
		}
	}
	if err := scheduler.authorizeRunSize(ctx, run, repository, size); err != nil {
		_ = logFile.Close()
		return scheduler.finishInfrastructureFailure(ctx, run, repository, err)
	}
	jobRequest.Size = size
	group, err := scheduler.pipelineConcurrencyGroup(ctx, jobRequest)
	if err != nil {
		_ = logFile.Close()
		return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("read pipeline concurrency group: %w", err))
	}
	if group != "" {
		// Recorded before admission so a run waiting on its group already
		// says which group it waits on (status, run_get, scheduling reason).
		if groupErr := scheduler.store.SetRunConcurrencyGroup(ctx, run.ID, group); groupErr != nil {
			_ = logFile.Close()
			return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("record concurrency group: %w", groupErr))
		}
		run.ConcurrencyGroup = group
		jobRequest.Run.ConcurrencyGroup = group
		jobRequest.ConcurrencyGroup = group
	}

	headSince, estimate := scheduler.admissionHistory(ctx, run)
	releaseAdmission, err := reservation.acquireRequest(ctx, admissionRequest{
		RunID: run.ID, RepoID: run.RepoID, Repo: repository.Name, Ref: run.Ref, Size: size, Group: group,
		RefKind: run.RefKind, SHA: run.SHA, HeadSince: headSince, Estimate: estimate,
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
		return err
	}
	defer releaseAdmission()
	if reservation.wasDelayed() {
		current, err = scheduler.store.Run(ctx, run.ID)
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
	current, err = scheduler.store.MarkRunAdmitted(ctx, run.ID)
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
		upstream, upstreamErr := scheduler.store.Upstream(ctx, repository.UpstreamID)
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
	if requirement.Contract != nil {
		stopped, err := scheduler.executeTrustedWithCapacity(ctx, jobRequest, requirement, logFile, reservation, require)
		if stopped {
			_ = logFile.Close()
			return nil
		}
		if err != nil {
			_ = logFile.Close()
			journal := scheduler.store.(vmrunner.KVMJournal)
			state, stateErr := journal.KVMPreflight(context.WithoutCancel(ctx), run.ID)
			if (stateErr != nil && !errors.Is(stateErr, store.ErrNotFound)) || (stateErr == nil && !state.Cleaned) {
				reservation.retain()
				return errors.Join(err, stateErr)
			}
			return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("trusted preflight: %w", err))
		}
		fresh, err := scheduler.trustedRequirement(ctx, repository, checkoutSHA)
		if err == nil {
			err = scheduler.trustedReceipt(ctx, repository, run, fresh)
		}
		if err != nil {
			_ = logFile.Close()
			return scheduler.finishInfrastructureFailure(ctx, run, repository, err)
		}
	}
	if !run.Credentialed && scheduler.pipelineCredentialed(ctx, jobRequest) {
		if credErr := scheduler.store.SetRunCredentialed(ctx, run.ID); credErr != nil {
			_ = logFile.Close()
			return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("mark run credentialed: %w", credErr))
		}
		run.Credentialed = true
	}
	if err = require(ctx); err != nil {
		_ = logFile.Close()
		return err
	}
	if requirement.Contract != nil {
		current, err = scheduler.store.SetRunJobName(ctx, run.ID, jobName)
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
	if fresh, err := scheduler.store.Run(ctx, run.ID); err != nil {
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

	switch {
	case run.Release:
		if scheduler.releaseJobs == nil {
			_ = logFile.Close()
			return scheduler.finishInfrastructureFailure(ctx, run, repository, errors.New("release Job capability is unavailable"))
		}
		err = scheduler.releaseJobs.CreateRelease(ctx, jobRequest)
	default:
		err = scheduler.jobs.CreateCI(ctx, jobRequest)
	}
	if err != nil {
		_ = logFile.Close()
		return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("create Job: %w", err))
	}
	current, err = scheduler.store.MarkRunSubmitted(ctx, run.ID)
	if err != nil {
		failure := errors.Join(fmt.Errorf("verify run after Job creation: %w", err), logFile.Close())
		cleanupCtx, cancelCleanup := boundedWorkspaceCleanupContext(ctx)
		deleteErr := scheduler.jobs.Delete(cleanupCtx, jobName, run.ID)
		cancelCleanup()
		if deleteErr != nil {
			// Keep the run active with its durable Job name. Returning the error
			// stops this owner; startup recovery will then create and retry the
			// exact cancellation instead of terminalizing an orphaned Job. The
			// admission stays held too: the Workflow may still be running.
			reservation.retain()
			return errors.Join(failure, fmt.Errorf("delete Job %s after run verification failure: %w", jobName, deleteErr))
		}
		return scheduler.finishInfrastructureFailure(ctx, run, repository, failure)
	}
	if current.Preempted() {
		_ = logFile.Close()
		cleanupCtx, cancelCleanup := boundedWorkspaceCleanupContext(ctx)
		if err := scheduler.jobs.Delete(cleanupCtx, jobName, run.ID); err != nil {
			// A cancellation pass may already have settled the obligation
			// before this Workflow existed; make it pending again so the next
			// pass or restart deletes it rather than orphaning it.
			rearmErr := scheduler.store.RearmRunCancellation(context.WithoutCancel(ctx), run.ID, jobName)
			cancelCleanup()
			reservation.retain()
			return errors.Join(fmt.Errorf("delete superseded Job %s: %w", jobName, err), rearmErr)
		}
		completeErr := scheduler.store.CompleteRunCancellation(cleanupCtx, run.ID, jobName)
		cancelCleanup()
		scheduler.signals.NotifyRun(run.ID)
		return completeErr
	}

	progressLog := newStepProgressLogWriter(
		newRunLogWriter(logFile, scheduler.maxRunLogBytes),
		func(event runprogress.Event) error { return scheduler.logs.AppendStepProgress(run.ID, event) },
	)
	// The plan is recorded here: after the pipeline object exists, so a run
	// that never reached submission carries no claim about work it would have
	// done, and before the engine's first observation, so the step list is
	// complete from the first poll rather than growing one row per Pod.
	if planErr := scheduler.recordStepPlan(ctx, jobRequest); planErr != nil {
		// Plan visibility is an observability record, never a gate. A run
		// whose plan cannot be enumerated still executes, and says so in its
		// own log rather than failing a build over a display concern.
		_, _ = fmt.Fprintf(progressLog, "oberth: step plan unavailable: %v\n", planErr)
	}
	jobResult, waitErr := scheduler.jobs.Wait(ctx, jobName, progressLog)
	if progressErr := progressLog.Close(); progressErr != nil && waitErr == nil {
		waitErr = progressErr
	}
	finalizeCtx := ctx
	var cancelFinalize context.CancelFunc
	if ctx.Err() != nil {
		finalizeCtx, cancelFinalize = context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelFinalize()
		// Check whether the Job already reached a terminal state before the
		// scheduler stopped. A completed Job must record its real result, not
		// a fabricated "interrupted."
		if reconciled, termErr := scheduler.jobs.TerminalResult(finalizeCtx, jobName); termErr == nil {
			jobResult = reconciled
			waitErr = nil
		} else {
			// Job not terminal, absent, or state unreadable at shutdown.
			// Requeue-with-supersede when eligible (issue #270): the durable
			// obligation owns the Job deletion, and whichever process next
			// runs a cancellation pass — the successor's startup in the
			// upgrade case — executes it before the replacement is claimed.
			if requeued, requeueErr := scheduler.store.RequeueStrandedRun(finalizeCtx, run.ID); requeueErr == nil {
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
			if deleteErr := scheduler.jobs.Delete(finalizeCtx, jobName, run.ID); deleteErr != nil {
				_ = logFile.Close()
				reservation.retain()
				return fmt.Errorf("cancel Job %s during scheduler shutdown: %w", jobName, deleteErr)
			}
			jobResult = JobResult{Status: model.RunInterrupted, Phase: "interrupted", Error: "scheduler stopped"}
			waitErr = nil
		}
	}
	closeErr := logFile.Close()
	if closeErr != nil && waitErr == nil {
		waitErr = closeErr
	}
	index, indexErr := scheduler.logs.BuildIndex(run.ID)
	if indexErr != nil && waitErr == nil {
		waitErr = indexErr
	}
	if waitErr != nil {
		// A transient Wait error (network blip, log reader failure) must not
		// terminalize the run without first checking whether the Workflow
		// actually reached a terminal state. If it did, use the real result
		// rather than fabricating a failure from the observation error.
		if recovered, termErr := scheduler.jobs.TerminalResult(finalizeCtx, jobName); termErr == nil {
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
			deleteErr := scheduler.jobs.Delete(deleteCtx, jobName, run.ID)
			cancelDelete()
			if deleteErr != nil {
				// The admission stays held with the run: the next member of
				// its concurrency group must not start beside a Workflow that
				// may still be alive.
				reservation.retain()
				return fmt.Errorf("delete Workflow %s before terminalizing failed run %s: %w",
					jobName, run.ID, deleteErr)
			}
			jobResult = JobResult{Status: model.RunFailed, Phase: "job", Error: waitErr.Error()}
		}
	}
	// The Workflow is now terminal or its deletion has been accepted, so its
	// weight and, for a grouped run, its concurrency group (#658) pass to the
	// next run here, before the slow finalization and publication below.
	// Releasing as soon as Wait returned handed the group on while a Workflow
	// whose Wait merely failed could still be running.
	releaseAdmission()
	if !validJobRunStatus(jobResult.Status) {
		jobResult = JobResult{Status: model.RunFailed, Phase: "job", Error: "Job returned an invalid terminal status"}
	}
	if gateErr := require(finalizeCtx); gateErr != nil {
		return gateErr
	}
	if err := scheduler.persistSteps(finalizeCtx, run.ID, jobResult.Steps, index); err != nil {
		jobResult = JobResult{Status: model.RunFailed, Phase: "collect", Error: err.Error()}
	}

	promotion, promotionErr := scheduler.promotionForRun(finalizeCtx, run)
	if promotionErr != nil {
		jobResult = JobResult{Status: model.RunFailed, Phase: "promotion", Error: promotionErr.Error()}
	}
	if jobResult.Status == model.RunPassed {
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
	if jobResult.Status != model.RunPassed {
		tail, tailErr := scheduler.failureExcerpt(run.ID, jobResult.FailedBurn, jobResult.FailedStep)
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
	_, err = scheduler.store.FinishRun(finalizeCtx, run.ID, model.RunResult{
		Status: jobResult.Status, Phase: jobResult.Phase, TestedSHA: checkoutSHA, BaseSHA: jobResult.BaseSHA,
		FailedBurn: jobResult.FailedBurn, FailedStep: jobResult.FailedStep, Error: jobResult.Error,
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
func (scheduler *Scheduler) recordStepPlan(ctx context.Context, request JobRequest) error {
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

func (scheduler *Scheduler) plannerFor(request JobRequest) (PipelinePlanner, bool) {
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
		if ordinal, planned := ordinals[result.Burn+"\x00"+result.Step]; planned {
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
			result.DeclaredSize = periapsis.M
		}
		result.LogStart, result.LogEnd = logOffsets(index, result.Burn, result.Step)
		if _, err := scheduler.store.PutStepResult(ctx, result); err != nil {
			return fmt.Errorf("persist step result %s/%s: %w", result.Burn, result.Step, err)
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
		ordinals[step.Burn+"\x00"+step.Step] = step.Ordinal
	}
	return ordinals
}

func (scheduler *Scheduler) promotionForRun(ctx context.Context, run model.Run) (model.Promotion, error) {
	if run.Trigger != "promotion" {
		return model.Promotion{}, nil
	}
	promotion, err := scheduler.store.PromotionByRun(ctx, run.ID)
	if err != nil {
		return model.Promotion{}, fmt.Errorf("load promotion for merged-tree run: %w", err)
	}
	if promotion.Status != model.PromotionPending || !sameOID(promotion.ResultSHA, run.TestedSHA) || !sameOID(promotion.PreviousSHA, run.BaseSHA) {
		return promotion, errors.New("service: promotion run does not match its immutable record")
	}
	return promotion, nil
}

func (scheduler *Scheduler) beginRunPublication(ctx context.Context, repository model.Repository, run model.Run, promotion model.Promotion) (model.Publication, error) {
	if promotion.ID != "" {
		return scheduler.store.BeginPublication(ctx, model.PublicationSpec{
			RepoID: repository.ID, RunID: run.ID, PromotionID: promotion.ID,
			RefKind: model.RefBranch, Ref: promotion.TargetRef,
			PreviousSHA: promotion.PreviousSHA, PreviousKnown: true,
			ResultSHA: promotion.ResultSHA, Actor: promotion.Actor,
		})
	}
	if _, err := canonicalPublicationRef(run.RefKind, run.Ref); err != nil {
		return model.Publication{}, err
	}
	return scheduler.store.BeginPublication(ctx, model.PublicationSpec{
		RepoID: repository.ID, RunID: run.ID, RefKind: run.RefKind, Ref: run.Ref,
		PreviousKnown: false, ResultSHA: run.SHA, Actor: run.Actor,
	})
}

// recoverPendingPublications resolves the full durable outbox at owner startup.
// Live retry wakes are ID-specific so they cannot seize another delivery.
func (scheduler *Scheduler) recoverPendingPublications(ctx context.Context) error {
	return scheduler.recoverPendingPublicationsWithGate(ctx, scheduler.requireMutation)
}

func (scheduler *Scheduler) recoverPendingPublicationsWithGate(ctx context.Context, require mutationRequirement) error {
	if err := require(ctx); err != nil {
		return err
	}
	pending, err := scheduler.store.PendingPublications(ctx)
	if err != nil {
		return fmt.Errorf("load pending publications: %w", err)
	}
	ids := make([]string, 0, len(pending))
	for _, publication := range pending {
		ids = append(ids, publication.ID)
	}
	return scheduler.recoverPublicationIDsWithGate(ctx, ids, require)
}

func (scheduler *Scheduler) recoverNotifiedPublicationsWithGate(ctx context.Context, require mutationRequirement) error {
	if err := require(ctx); err != nil {
		return err
	}
	return scheduler.recoverPublicationIDsWithGate(ctx, scheduler.takePublicationRecoveries(), require)
}

func (scheduler *Scheduler) recoverPublicationIDsWithGate(ctx context.Context, ids []string, require mutationRequirement) error {
	var recoveryErr error
	for _, id := range ids {
		finalization, err := scheduler.deliverPublicationWithGate(ctx, id, require)
		if err != nil {
			recoveryErr = errors.Join(recoveryErr, fmt.Errorf("recover publication %s: %w", id, err))
			continue
		}
		var cleanupErr error
		if finalization.Run.ID != "" {
			cleanupErr = scheduler.workspaces.cleanupRun(ctx, finalization.Run.ID)
		}
		if finalization.Promotion.ID != "" {
			cleanupErr = errors.Join(cleanupErr, scheduler.workspaces.cleanupPromotion(ctx, finalization.Promotion.ID))
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
	select {
	case <-ctx.Done():
		return model.PublicationFinalization{}, ctx.Err()
	case <-scheduler.deliveryPermit:
	}
	defer func() { scheduler.deliveryPermit <- struct{}{} }()
	if err := require(ctx); err != nil {
		return model.PublicationFinalization{}, err
	}
	publication, err := scheduler.store.Publication(ctx, publicationID)
	if err != nil {
		return model.PublicationFinalization{}, fmt.Errorf("load publication %s: %w", publicationID, err)
	}
	if publication.Status.Terminal() {
		return scheduler.loadPublicationFinalization(ctx, publication)
	}
	repository, err := scheduler.store.Repository(ctx, publication.RepoID)
	if err != nil {
		return model.PublicationFinalization{}, fmt.Errorf("load repository for publication %s: %w", publication.ID, err)
	}
	if err := require(ctx); err != nil {
		return model.PublicationFinalization{}, err
	}
	policyErr := scheduler.requirePublicationTestPolicy(ctx, repository, publication)
	if err := require(ctx); err != nil {
		return model.PublicationFinalization{}, err
	}
	if policyErr != nil {
		finalization, err := scheduler.store.FinalizePublication(ctx, publication.ID, model.PublicationFailed, policyErr.Error())
		if err != nil {
			return model.PublicationFinalization{}, err
		}
		return finalization, scheduler.finishPublication(ctx, repository, finalization)
	}
	publicationInput := repository.Name
	if upstream, upstreamErr := scheduler.store.Upstream(ctx, repository.UpstreamID); upstreamErr == nil {
		publicationInput = upstream.QualifiedRepo(repository.Name)
	}
	finalization, err = deliverPublication(ctx, scheduler.store, scheduler.git, scheduler.auditor, repository, publicationInput, publication)
	if err != nil {
		return model.PublicationFinalization{}, err
	}
	finalizedHere = true
	if err := scheduler.finishPublication(ctx, repository, finalization); err != nil {
		return model.PublicationFinalization{}, err
	}
	return finalization, nil
}

func (scheduler *Scheduler) loadPublicationFinalization(
	ctx context.Context,
	publication model.Publication,
) (model.PublicationFinalization, error) {
	finalization := model.PublicationFinalization{Publication: publication}
	var err error
	if publication.RunID != "" {
		finalization.Run, err = scheduler.store.Run(ctx, publication.RunID)
		if err != nil {
			return model.PublicationFinalization{}, fmt.Errorf("load finalized publication run %s: %w", publication.RunID, err)
		}
	}
	if publication.PromotionID != "" {
		finalization.Promotion, err = scheduler.store.Promotion(ctx, publication.PromotionID)
		if err != nil {
			return model.PublicationFinalization{}, fmt.Errorf("load finalized publication promotion %s: %w", publication.PromotionID, err)
		}
	}
	return finalization, nil
}

func (scheduler *Scheduler) finishPublication(_ context.Context, _ model.Repository, finalization model.PublicationFinalization) error {
	if finalization.Promotion.ID != "" {
		scheduler.signals.NotifyPromotion(finalization.Promotion.ID)
	}
	if finalization.Run.ID == "" {
		return nil
	}
	scheduler.signals.NotifyRun(finalization.Run.ID)
	return nil
}

func (scheduler *Scheduler) finishInfrastructureFailure(ctx context.Context, run model.Run, _ model.Repository, failure error) error {
	_, err := scheduler.store.FinishRun(ctx, run.ID, model.RunResult{Status: model.RunFailed, Phase: "infrastructure", Error: failure.Error()})
	if errors.Is(err, store.ErrInvalidState) {
		scheduler.signals.NotifyRun(run.ID)
		return nil
	}
	if err != nil {
		return errors.Join(failure, err)
	}
	if run.Trigger == "promotion" {
		promotion, promotionErr := scheduler.store.PromotionByRun(ctx, run.ID)
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
// real durable result; runs whose Jobs are absent or still active are
// interrupted. This must run before completePendingCancellationsWithGate,
// which would delete the K8s Jobs before their terminal state can be read.
func (scheduler *Scheduler) reconcileStrandedRunsWithGate(ctx context.Context, require mutationRequirement) error {
	if err := require(ctx); err != nil {
		return err
	}
	runs, err := scheduler.store.RunningRunsWithJobs(ctx)
	if err != nil {
		return fmt.Errorf("load stranded runs for reconciliation: %w", err)
	}
	for _, run := range runs {
		reconciled, termErr := scheduler.jobs.TerminalResult(ctx, run.JobName)
		if termErr == nil {
			if err := scheduler.finalizeReconciledRun(ctx, run, reconciled, require); err != nil {
				return err
			}
		} else {
			requeued, requeueErr := scheduler.store.RequeueStrandedRun(ctx, run.ID)
			if requeueErr == nil {
				// The stranded Job's deletion is now owned by the durable
				// supersede obligation the requeue recorded; the cancellation
				// pass that runs immediately after reconciliation executes it
				// before any new claim can start the replacement run
				// (issue #270). The obligation must stay pending here, so the
				// loop tail's CompleteRunCancellation is skipped.
				scheduler.signals.NotifyRun(run.ID)
				scheduler.signals.NotifyRun(requeued.ID)
				cleanupCtx, cancelCleanup := boundedWorkspaceCleanupContext(ctx)
				_ = scheduler.workspaces.cleanupRun(cleanupCtx, run.ID)
				cancelCleanup()
				continue
			}
			if !errors.Is(requeueErr, store.ErrRequeueIneligible) {
				return fmt.Errorf("requeue stranded run %s: %w", run.ID, requeueErr)
			}
			deleteCtx, cancelDelete := boundedWorkspaceCleanupContext(ctx)
			deleteErr := scheduler.jobs.Delete(deleteCtx, run.JobName, run.ID)
			cancelDelete()
			if deleteErr != nil {
				// Deletion failed — the Workflow may still be running with
				// credentials. Leave the run owned for the next restart pass
				// rather than recording interrupted while the workload lives.
				return fmt.Errorf("delete stranded Workflow %s for run %s: %w", run.JobName, run.ID, deleteErr)
			}
			errMsg := "job not in terminal state at restart; result unrecoverable"
			if !errors.Is(termErr, ErrJobNotTerminal) {
				errMsg = fmt.Sprintf("job state unreadable at restart: %v", termErr)
			}
			if gateErr := require(ctx); gateErr != nil {
				return gateErr
			}
			_, finishErr := scheduler.store.FinishRun(ctx, run.ID, model.RunResult{
				Status: model.RunInterrupted, Phase: "interrupted", Error: errMsg,
			})
			if finishErr != nil && !errors.Is(finishErr, store.ErrInvalidState) {
				return fmt.Errorf("interrupt stranded run %s: %w", run.ID, finishErr)
			}
			if run.Trigger == "promotion" {
				if promotion, promotionErr := scheduler.store.PromotionByRun(ctx, run.ID); promotionErr == nil {
					scheduler.signals.NotifyPromotion(promotion.ID)
				}
			}
			scheduler.signals.NotifyRun(run.ID)
		}
		_ = scheduler.store.CompleteRunCancellation(ctx, run.ID, run.JobName)
		cleanupCtx, cancelCleanup := boundedWorkspaceCleanupContext(ctx)
		_ = scheduler.workspaces.cleanupRun(cleanupCtx, run.ID)
		if run.Trigger == "promotion" {
			if promotion, promotionErr := scheduler.store.PromotionByRun(cleanupCtx, run.ID); promotionErr == nil {
				_ = scheduler.workspaces.cleanupPromotion(cleanupCtx, promotion.ID)
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
	repository, err := scheduler.store.Repository(ctx, run.RepoID)
	if err != nil {
		return scheduler.finishInfrastructureFailure(ctx, run, model.Repository{}, fmt.Errorf("load reconciled run repository: %w", err))
	}
	// Build the log index from whatever partial log the reconciled run left
	// behind, so per-step logs remain readable (#437). The log file may not
	// exist if the run never started streaming; an empty index is acceptable.
	index, indexErr := scheduler.logs.BuildIndex(run.ID)
	if indexErr != nil {
		index = runlog.Index{}
	}
	if err := scheduler.persistSteps(ctx, run.ID, jobResult.Steps, index); err != nil {
		jobResult = JobResult{Status: model.RunFailed, Phase: "collect", Error: err.Error()}
	}
	promotion, promotionErr := scheduler.promotionForRun(ctx, run)
	if promotionErr != nil {
		jobResult = JobResult{Status: model.RunFailed, Phase: "promotion", Error: promotionErr.Error()}
	}
	if jobResult.Status == model.RunPassed {
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
	_, err = scheduler.store.FinishRun(ctx, run.ID, model.RunResult{
		Status: jobResult.Status, Phase: jobResult.Phase,
		FailedBurn: jobResult.FailedBurn, FailedStep: jobResult.FailedStep,
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

// RunSizeLimiter reports the largest admission size a repository's runs may
// be admitted at (#1021): L unless an administrator granted XL.
type RunSizeLimiter interface {
	MaxRunSize(context.Context, int64) (periapsis.Size, error)
}

// authorizeRunSize refuses a branch, promotion or schedule run whose resolved
// size exceeds its repository's size grant, before it takes any weight: one
// repository's document declaring XL would otherwise make every other
// repository wait for a full drain. A tag run keeps its reviewed
// release.yaml size. A store without the capability (test doubles) limits
// nothing; the production store always has it.
func (scheduler *Scheduler) authorizeRunSize(ctx context.Context, run model.Run, repository model.Repository, size periapsis.Size) error {
	limiter, ok := scheduler.store.(RunSizeLimiter)
	if run.Release || !ok {
		return nil
	}
	maximum, err := limiter.MaxRunSize(ctx, run.RepoID)
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

func (scheduler *Scheduler) pipelineSize(ctx context.Context, request JobRequest) (periapsis.Size, error) {
	if request.Run.Release {
		if sizer, ok := scheduler.releaseJobs.(PipelineSizer); ok {
			return sizer.PipelineSize(ctx, request)
		}
	}
	if sizer, ok := scheduler.jobs.(PipelineSizer); ok {
		return sizer.PipelineSize(ctx, request)
	}
	return periapsis.M, nil
}

// pipelineConcurrencyGroup reads the concurrency group the run's own pipeline
// document declares, selecting the engine exactly as pipelineSize does (the
// engine derives the document, build.yaml or release.yaml, from the durable
// run). An engine without the capability runs every pipeline ungrouped.
func (scheduler *Scheduler) pipelineConcurrencyGroup(ctx context.Context, request JobRequest) (string, error) {
	if request.Run.Release {
		if grouper, ok := scheduler.releaseJobs.(PipelineConcurrencyGrouper); ok {
			return grouper.PipelineConcurrencyGroup(ctx, request)
		}
	}
	if grouper, ok := scheduler.jobs.(PipelineConcurrencyGrouper); ok {
		return grouper.PipelineConcurrencyGroup(ctx, request)
	}
	return "", nil
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
func (scheduler *Scheduler) pipelineCredentialed(ctx context.Context, request JobRequest) bool {
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

func logOffsets(index runlog.Index, burn, step string) (int64, int64) {
	var start, end int64
	found := false
	for _, value := range index.Ranges {
		if value.Burn != burn || value.Step != step {
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

func deterministicJobName(repository, runID string) (string, error) {
	if strings.TrimSpace(runID) == "" || len(runID) > 48 || runID[0] == '-' || runID[len(runID)-1] == '-' {
		return "", errors.New("service: run ID cannot form a deterministic Job name")
	}
	for _, character := range runID {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return "", errors.New("service: run ID cannot form a deterministic Job name")
		}
	}
	repositoryFragment := jobNameFragment(repository)
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

func jobNameFragment(value string) string {
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
