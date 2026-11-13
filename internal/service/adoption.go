package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type adoptedRun struct {
	run         model.Run
	reservation *submissionOwner
}

// Workflow adoption keeps the durable run classification; the engine checks
// the exact trigger, source and credential-bearing spec before resuming it.
func workflowRunAdoptable(run model.Run) bool {
	// Keep the persisted trigger and credential classification. The engine's
	// exact Workflow binding checks apply to ordinary tags as well as releases.
	return run.RefKind == model.RefBranch || run.RefKind == model.RefTag
}

// adoptionFailure names the exact Workflow that could not be recovered.
// A binding refusal fails only its run; no deletion or replacement is authorized.
// Workflow-read failures remain startup errors rather than inferred results.
func adoptionFailure(adopter JobAdopter, run model.Run, err error) error {
	namespace := adopter.WorkflowNamespace()
	if target, ok := adopter.(interface {
		WorkflowNamespaceForRun(context.Context, model.Run) (string, error)
	}); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		selected, selectErr := target.WorkflowNamespaceForRun(ctx, run)
		if selectErr != nil {
			failure := fmt.Errorf("adopt run %s: Workflow %s: original execution target unavailable: %w", run.ID, run.WorkflowName, err)
			log.Printf("ERROR: %v", failure)
			return failure
		}
		namespace = selected
	}
	failure := fmt.Errorf("adopt run %s: Workflow %s/%s: %w", run.ID, namespace, run.WorkflowName, err)
	if errors.Is(err, ErrAdoptionRefused) {
		failure = fmt.Errorf("%w; recovery refused for this run; inspect the original Workflow (kubectl -n %s get workflow %s); no automatic deletion or resubmission",
			failure, namespace, run.WorkflowName)
	}
	log.Printf("ERROR: %v", failure)
	return failure
}

func (scheduler *Scheduler) handleAdoptionFailure(ctx context.Context, adopter JobAdopter, run model.Run, cause error, require mutationRequirement) error {
	failure := adoptionFailure(adopter, run, cause)
	if !errors.Is(cause, ErrAdoptionRefused) {
		return failure
	}
	message := failure.Error()
	if errors.Is(cause, store.ErrExecutionTargetQuarantined) {
		message = "execution target quarantined: " + message
	}
	return scheduler.finalizeReconciledRun(ctx, run, JobResult{Status: model.RunFailed, Phase: "infrastructure", Error: message}, require)
}

// adopt restores submission ownership for work already running.
func (gate *submissionRegistry) adopt(run model.Run) (*submissionOwner, error) {
	order := run.AdmissionSequence
	if order <= 0 {
		order = run.QueueSequence
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if previous, ok := gate.owners[run.QueueSequence]; ok {
		if previous.runID != run.ID || previous.order != order || !previous.granted || previous.repoID != run.RepoID {
			return nil, fmt.Errorf("%w: conflicting adopted admission", ErrAdoptionRefused)
		}
		return previous, nil
	}
	reservation := &submissionOwner{withdrawn: make(chan struct{}), gate: gate, sequence: run.QueueSequence, order: order, runID: run.ID, repoID: run.RepoID, granted: true, submitted: true}
	gate.owners[run.QueueSequence] = reservation
	return reservation, nil
}

func (scheduler *Scheduler) resumeAdoptedRun(ctx context.Context, run model.Run, require mutationRequirement, reservation *submissionOwner) error {
	defer reservation.cancel()
	// Observation now owns this grant; uncertain-create custody has ended.
	scheduler.submissions.mu.Lock()
	reservation.retained = false
	scheduler.submissions.mu.Unlock()
	if err := require(ctx); err != nil {
		reservation.retain()
		return err
	}
	repository, err := scheduler.database().Repository(ctx, run.RepoID)
	if err != nil {
		reservation.retain()
		return err
	}

	current, err := scheduler.database().Run(ctx, run.ID)
	if err != nil {
		reservation.retain()
		return err
	}
	if current.Preempted() {
		if err := scheduler.completePendingCancellationsWithGate(ctx, require); err != nil {
			reservation.retain()
			return scheduler.deferRefusedDeletions(ctx, run.ID, err)
		}
		return nil
	}
	file, events, retained, err := scheduler.logs.Resume(run.ID)
	if err != nil {
		reservation.retain()
		return err
	}
	// A previous owner can stop between Workflow creation and this transition.
	if run.Phase == "waiting" || isPendingCreate(run) {
		run, err = scheduler.database().MarkRunSubmitted(ctx, run.ID)
		if err != nil {
			_ = file.Close()
			reservation.retain()
			return err
		}
	}
	if run.Status == model.RunRunning && run.Phase == "running" {
		// The matching Workflow is adopted and submission is durable. A
		// failed unlink is retried by terminal workspace recovery.
		_ = os.Remove(scheduler.createAdmissionPath(run.ID))
	}
	err = scheduler.observeRun(ctx, run, repository, require, reservation, runObservation{
		file: file, events: events, retained: retained, adopted: true,
	})
	cleanupCtx, cancel := boundedWorkspaceCleanupContext(ctx)
	defer cancel()
	current, lookupErr := scheduler.database().Run(cleanupCtx, run.ID)
	if lookupErr != nil || !current.Status.Terminal() {
		return errors.Join(err, lookupErr)
	}
	cleanupErr := scheduler.workspaces.cleanupRun(cleanupCtx, run.ID)
	if run.Trigger == "promotion" {
		promotion, promotionErr := scheduler.database().PromotionByRun(cleanupCtx, run.ID)
		if promotionErr != nil {
			cleanupErr = errors.Join(cleanupErr, promotionErr)
		} else {
			cleanupErr = errors.Join(cleanupErr, scheduler.workspaces.cleanupPromotion(cleanupCtx, promotion.ID))
		}
	}
	return errors.Join(err, cleanupErr)
}
