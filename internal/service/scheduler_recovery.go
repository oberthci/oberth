package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func (scheduler *Scheduler) reconcileStrandedRunsWithGate(ctx context.Context, require mutationRequirement, adopt func(model.Run) error) error {
	if err := require(ctx); err != nil {
		return err
	}
	if adopter, ok := scheduler.jobs.(JobAdopter); ok {
		if err := adopter.WaitForWatchSync(ctx); err != nil {
			return err
		}
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
			continue
		}
		if restored[run.ID] && (isPendingCreate(run) || run.Phase == "waiting") {
			if err := scheduler.cancelRestoredRun(ctx, run.ID, require); err != nil {
				return err
			}
			continue
		}
		if adopter, ok := scheduler.jobs.(JobAdopter); ok && workflowRunAdoptable(run) {
			found, readErr := scheduler.adoptStartupWorkflow(ctx, adopter, run)
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

// Startup waits for uncertain native observations, never inferring a verdict
// from an API outage. Binding refusals remain permanent for that run alone.
func (scheduler *Scheduler) adoptStartupWorkflow(ctx context.Context, adopter JobAdopter, run model.Run) (bool, error) {
	delay := databaseRetryMinimum
	var nextLog time.Time
	for {
		found, err := adopter.Adopt(ctx, run)
		if err == nil {
			return found, nil
		}
		class, unavailable := startupAdoptionErrorClass(err)
		if !unavailable {
			if class == "binding_refused" {
				return false, err
			}
			return false, fmt.Errorf("startup adoption class=%s: %w", class, err)
		}
		now := time.Now()
		if !now.Before(nextLog) {
			log.Printf("WARN: waiting to adopt run %s: Workflow %s/%s observation unavailable (class=%s); startup recovery pending", run.ID, adopter.WorkflowNamespace(), run.WorkflowName, class)
			nextLog = now.Add(30 * time.Second)
		}
		if ctx.Err() != nil {
			return false, fmt.Errorf("adopt run %s: Workflow %s/%s: %w", run.ID, adopter.WorkflowNamespace(), run.WorkflowName, errors.Join(ctx.Err(), err))
		}
		if waitErr := waitDatabaseRetry(ctx, ctx, delay); waitErr != nil {
			return false, fmt.Errorf("adopt run %s: Workflow %s/%s: %w", run.ID, adopter.WorkflowNamespace(), run.WorkflowName, errors.Join(waitErr, err))
		}
		delay = min(2*delay, databaseRetryMaximum)
	}
}

// Classification logs no server response text or credential-bearing error body.
func startupAdoptionErrorClass(err error) (string, bool) {
	var network net.Error
	var status apierrors.APIStatus
	switch {
	case errors.Is(err, ErrAdoptionRefused), errors.Is(err, argoworkflow.ErrWorkflowCapabilityRequired):
		return "binding_refused", false
	case errors.Is(err, context.Canceled):
		return "canceled", false
	case apierrors.IsForbidden(err):
		return "forbidden", false
	case apierrors.IsUnauthorized(err):
		return "unauthorized", false
	case errors.Is(err, ErrUnavailable):
		return "watch_unavailable", true
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline", true
	case apierrors.IsTooManyRequests(err):
		return "throttled", true
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		return "api_timeout", true
	case errors.As(err, &status) && status.Status().Code >= 500 && status.Status().Code <= 599:
		return "api_server", true
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "transport_eof", true
	case errors.As(err, &network):
		return "transport", true
	default:
		return "permanent", false
	}
}
