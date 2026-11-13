package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
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
	if errors.Is(cause, argoworkflow.ErrWorkflowCapabilityRequired) {
		return scheduler.failRetiredRun(ctx, run, require)
	}
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
		if previous.runID != run.ID || previous.order != order || !previous.adopted || previous.repoID != run.RepoID {
			return nil, fmt.Errorf("%w: conflicting adopted run", ErrAdoptionRefused)
		}
		return previous, nil
	}
	reservation := &submissionOwner{withdrawn: make(chan struct{}), gate: gate, sequence: run.QueueSequence, order: order, runID: run.ID, repoID: run.RepoID, adopted: true}
	gate.owners[run.QueueSequence] = reservation
	return reservation, nil
}

func (scheduler *Scheduler) resumeAdoptedRun(ctx context.Context, run model.Run, require mutationRequirement, reservation *submissionOwner) error {
	return scheduler.resumeObservedRun(ctx, run, require, reservation, false)
}

// Retired execution is terminal, rather than a reason to stop the new owner.
// Terminate only the owned Argo Workflow; operators own any external resources.
func (scheduler *Scheduler) failRetiredRun(ctx context.Context, run model.Run, require mutationRequirement) error {
	if err := require(ctx); err != nil {
		return err
	}
	// Refused recovery cannot supply exit identities, so native Terminate skips exits.
	scheduler.deferCreateCleanup(ctx, run, run.WorkflowName, argoworkflow.ErrWorkflowCapabilityRequired)
	if err := scheduler.jobs.Terminate(ctx, run.WorkflowName, run.ID); err != nil {
		log.Printf("WARN: retired Workflow termination remains pending: %v", err)
	}
	return nil
}
