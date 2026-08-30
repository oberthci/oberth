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
	"time"

	"github.com/oberthci/oberth/internal/gitoid"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/periapsis"
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
	MaxConcurrent   int
	MaxRunLogBytes  int64
	MutationGate    func(context.Context) error
	// SuppressGreenPublication stops an ordinary green branch run from
	// force-syncing itself to the upstream forge. The zero value publishes,
	// which is the original behaviour and keeps every existing caller correct
	// without naming the field. Set it to make the gate advisory: the run goes
	// green, the verdict is recorded, and nothing reaches the forge until
	// someone asks for it. Promotion runs publish either way, because a
	// promotion's whole purpose is to publish.
	SuppressGreenPublication bool
}

type Scheduler struct {
	store                    SchedulerStore
	git                      WorkspaceGit
	logs                     LogStore
	jobs                     JobController
	releaseJobs              ReleaseJobController
	auditor                  Auditor
	signals                  *Signals
	workspaces               *workspaceLifecycle
	workspaceRoot            string
	maxConcurrent            int
	admission                *weightedAdmission
	maxRunLogBytes           int64
	mutationGate             func(context.Context) error
	wake                     chan struct{}
	publicationWake          chan struct{}
	publicationMu            sync.Mutex
	publicationIDs           []string
	publicationSet           map[string]struct{}
	deliveryPermit           chan struct{}
	suppressGreenPublication bool
}

func NewScheduler(config SchedulerConfig) (*Scheduler, error) {
	if config.Store == nil || config.Git == nil || config.Logs == nil || config.Jobs == nil || config.Auditor == nil {
		return nil, errors.New("service: scheduler store, Git, logs, jobs, and auditor are required")
	}
	if config.MaxConcurrent <= 0 {
		config.MaxConcurrent = 3
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
	return &Scheduler{
		suppressGreenPublication: config.SuppressGreenPublication,
		store:                    config.Store, git: config.Git, logs: config.Logs, jobs: config.Jobs,
		releaseJobs: config.ReleaseJobs,
		auditor:     config.Auditor, signals: signals,
		workspaces: workspaces, workspaceRoot: workspaceRoot, maxConcurrent: config.MaxConcurrent,
		admission:      newWeightedAdmissionForJobs(config.MaxConcurrent),
		maxRunLogBytes: config.MaxRunLogBytes, mutationGate: mutationGate,
		wake: make(chan struct{}, 1), publicationWake: make(chan struct{}, 1),
		publicationSet: make(map[string]struct{}), deliveryPermit: deliveryPermit,
	}, nil
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

// Run owns oldest-eligible claims for one server process and fills at most
// MaxConcurrent slots. A pending publication temporarily gates only its exact
// repository/ref; queue changes and completions wake the scheduler through
// bounded channels.
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
	scheduler.signalQueue()
	completed := make(chan error, scheduler.maxConcurrent)
	active := 0
	for {
		for active < scheduler.maxConcurrent {
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
			reservation := scheduler.admission.reserve(run.QueueSequence)
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
	reservation := scheduler.admission.reserve(run.QueueSequence)
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
	if err := scheduler.git.Checkout(ctx, repository.Name, checkoutSHA, sourceDir); err != nil {
		return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("checkout run workspace: %w", err))
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
	current, err := scheduler.store.SetRunJobName(ctx, run.ID, jobName)
	if err != nil {
		_ = logFile.Close()
		return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("persist deterministic Job intent: %w", err))
	}
	if current.Status == model.RunInterrupted && current.SupersededBy != "" {
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
	jobRequest.Size = size
	if !run.Credentialed && scheduler.pipelineCredentialed(ctx, jobRequest) {
		if credErr := scheduler.store.SetRunCredentialed(ctx, run.ID); credErr != nil {
			_ = logFile.Close()
			return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("mark run credentialed: %w", credErr))
		}
		run.Credentialed = true
	}
	releaseAdmission, err := reservation.acquire(ctx, size)
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
		if current.Status == model.RunInterrupted && current.SupersededBy != "" {
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
	{
		upstream, upstreamErr := scheduler.store.Upstream(ctx, repository.UpstreamID)
		if upstreamErr != nil {
			_ = logFile.Close()
			return scheduler.finishInfrastructureFailure(ctx, run, repository, fmt.Errorf("load repository upstream: %w", upstreamErr))
		}
		jobRequest.UpstreamName = upstream.Name
		jobRequest.UpstreamOrg = upstream.Org()
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
	current, err = scheduler.store.Run(ctx, run.ID)
	if err != nil {
		failure := errors.Join(fmt.Errorf("verify run after Job creation: %w", err), logFile.Close())
		cleanupCtx, cancelCleanup := boundedWorkspaceCleanupContext(ctx)
		deleteErr := scheduler.jobs.Delete(cleanupCtx, jobName, run.ID)
		cancelCleanup()
		if deleteErr != nil {
			// Keep the run active with its durable Job name. Returning the error
			// stops this owner; startup recovery will then create and retry the
			// exact cancellation instead of terminalizing an orphaned Job.
			return errors.Join(failure, fmt.Errorf("delete Job %s after run verification failure: %w", jobName, deleteErr))
		}
		return scheduler.finishInfrastructureFailure(ctx, run, repository, failure)
	}
	if current.Status == model.RunInterrupted && current.SupersededBy != "" {
		_ = logFile.Close()
		cleanupCtx, cancelCleanup := boundedWorkspaceCleanupContext(ctx)
		if err := scheduler.jobs.Delete(cleanupCtx, jobName, run.ID); err != nil {
			cancelCleanup()
			return fmt.Errorf("delete superseded Job %s: %w", jobName, err)
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
	releaseAdmission()
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
			// Job not terminal, absent, or state unreadable — delete and interrupt.
			if deleteErr := scheduler.jobs.Delete(finalizeCtx, jobName, run.ID); deleteErr != nil {
				_ = logFile.Close()
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
				return fmt.Errorf("delete Workflow %s before terminalizing failed run %s: %w",
					jobName, run.ID, deleteErr)
			}
			jobResult = JobResult{Status: model.RunFailed, Phase: "job", Error: waitErr.Error()}
		}
	}
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
	// A promotion always publishes: publishing the merged result is what a
	// promotion is for, and its caller supplied explicit integration authority.
	// An ordinary branch run publishes only when the operator asked for it.
	if jobResult.Status == model.RunPassed && (!scheduler.suppressGreenPublication || promotion.ID != "") {
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
		tail, tailErr := scheduler.logs.Tail(run.ID, maximumCIIssueTailBytes)
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

// PublishRun force-syncs an already-green branch run to the upstream forge on
// request. It exists because --publish-on-green=false makes the gate advisory:
// the verdict is recorded and nothing is pushed, so something has to be able to
// say "now".
//
// It reuses the ordinary publication path rather than adding a second way to
// reach the forge, so the durable outbox, the audit chain and the fail-closed
// mutation gate all apply exactly as they do for an automatic publish. The only
// difference is who decided.
//
// Refuses anything but a terminal green ordinary branch run. A failed run has
// nothing to publish, a promotion published itself, and a running one has no
// result yet.
func (scheduler *Scheduler) PublishRun(ctx context.Context, runID string) error {
	run, err := scheduler.store.Run(ctx, runID)
	if err != nil {
		return fmt.Errorf("load run for publication: %w", err)
	}
	if run.Status != model.RunPassed {
		return fmt.Errorf("service: run %s is %s; only a green run can be published", runID, run.Status)
	}
	if run.Trigger == "promotion" {
		return fmt.Errorf("service: run %s is a promotion and published itself", runID)
	}
	repository, err := scheduler.store.Repository(ctx, run.RepoID)
	if err != nil {
		return fmt.Errorf("load repository for publication: %w", err)
	}
	publication, err := scheduler.beginRunPublication(ctx, repository, run, model.Promotion{})
	if err != nil {
		return fmt.Errorf("begin publication: %w", err)
	}
	if _, err := scheduler.deliverPublicationWithGate(ctx, publication.ID, scheduler.requireMutation); err != nil {
		return fmt.Errorf("deliver publication %s: %w", publication.ID, err)
	}
	return nil
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
) (model.PublicationFinalization, error) {
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
	finalization, err := deliverPublication(ctx, scheduler.store, scheduler.git, scheduler.auditor, repository, publication)
	if err != nil {
		return model.PublicationFinalization{}, err
	}
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
	if err := scheduler.persistSteps(ctx, run.ID, jobResult.Steps, runlog.Index{}); err != nil {
		jobResult = JobResult{Status: model.RunFailed, Phase: "collect", Error: err.Error()}
	}
	promotion, promotionErr := scheduler.promotionForRun(ctx, run)
	if promotionErr != nil {
		jobResult = JobResult{Status: model.RunFailed, Phase: "promotion", Error: promotionErr.Error()}
	}
	// Mirrors the gate in the primary finalize path. Without it a restart
	// would publish a run the operator deliberately left unpublished.
	if jobResult.Status == model.RunPassed && (!scheduler.suppressGreenPublication || promotion.ID != "") {
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

// pipelineCredentialed reports whether the run's pipeline declares secret-store
// paths that make it a credentialed run. Releases are always credentialed
// (already set at enqueue time); this method detects CI pipelines with declared
// secrets so the run can be marked before the log writer is created.
func (scheduler *Scheduler) pipelineCredentialed(ctx context.Context, request JobRequest) bool {
	if request.Run.Release || request.Run.Credentialed {
		return true
	}
	if detector, ok := scheduler.jobs.(PipelineCredentialDetector); ok {
		credentialed, err := detector.PipelineCredentialed(ctx, request)
		if err != nil {
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
