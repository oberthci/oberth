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
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

type adoptedRun struct {
	run         model.Run
	reservation *admissionReservation
}

// Workflow adoption keeps the durable run classification; the engine checks
// the exact trigger, source and credential-bearing spec before resuming it.
func workflowRunAdoptable(run model.Run) bool {
	// Keep the persisted trigger and credential classification. The engine's
	// exact Workflow binding checks apply to ordinary tags as well as releases.
	return run.RefKind == model.RefBranch || run.RefKind == model.RefTag
}

// adoptionFailure names the exact Workflow startup could not adopt, so the
// operator knows which object to delete. A refused Workflow keeps startup
// stopped until it is deleted; recovery then takes the missing-Workflow
// fallback and re-queues the run. A failed read names the same object but
// asks for nothing: the next start retries it.
func adoptionFailure(adopter JobAdopter, run model.Run, err error) error {
	namespace := adopter.WorkflowNamespace()
	if target, ok := adopter.(interface {
		WorkflowNamespaceForRun(context.Context, model.Run) (string, error)
	}); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		selected, selectErr := target.WorkflowNamespaceForRun(ctx, run)
		if selectErr != nil {
			return fmt.Errorf("adopt run %s: original execution target unavailable: %w", run.ID, err)
		}
		namespace = selected
	}
	failure := fmt.Errorf("adopt run %s: Workflow %s/%s: %w", run.ID, namespace, run.WorkflowName, err)
	if errors.Is(err, ErrAdoptionRefused) {
		failure = fmt.Errorf("%w; startup stops until it is deleted (kubectl -n %s delete workflow %s); recovery retains the original execution target",
			failure, namespace, run.WorkflowName)
	}
	log.Printf("ERROR: %v", failure)
	return failure
}

// adopt restores grants for work already running, even if the new owner's
// configured capacity is smaller. New admissions wait until it has drained.
func (gate *weightedAdmission) adopt(run model.Run, size pipeline.Size, pods ...*argoworkflow.PodDemand) (*admissionReservation, error) {
	var peak *argoworkflow.PodDemand
	if len(pods) > 0 && pods[0] != nil {
		if !pods[0].Valid() {
			return nil, fmt.Errorf("%w: invalid adopted pod demand", ErrAdoptionRefused)
		}
		copy := *pods[0]
		copy.DeclaredTotal = pods[0].DeclaredTotal.Clone()
		peak = &copy
	}
	if (!gate.workflowOnly && !size.Valid()) || (run.ConcurrencyGroup != "" && !argoworkflow.ValidConcurrencyGroup(run.ConcurrencyGroup)) {
		return nil, fmt.Errorf("%w: invalid adopted resource contract", ErrAdoptionRefused)
	}
	order := run.AdmissionSequence
	if order <= 0 {
		order = run.QueueSequence
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if previous, ok := gate.reservations[run.QueueSequence]; ok {
		if previous.runID != run.ID || previous.order != order || !previous.granted || previous.size != size.Effective() || previous.groupName != run.ConcurrencyGroup || max(1, previous.groupLimit) != max(1, run.ConcurrencyLimit) || previous.repoID != run.RepoID || !samePodDemand(previous.pods, peak) {
			return nil, fmt.Errorf("%w: conflicting adopted admission", ErrAdoptionRefused)
		}
		return previous, nil
	}
	group := ""
	if run.ConcurrencyGroup != "" {
		group = concurrencyGroupKey(run.RepoID, run.ConcurrencyGroup)
		limit := max(1, run.ConcurrencyLimit)
		for _, holder := range gate.groups[group] {
			if max(1, holder.groupLimit) != limit {
				return nil, fmt.Errorf("%w: conflicting adopted concurrency group limits", ErrAdoptionRefused)
			}
			// Before shared groups, independent repositories could each own
			// one grant. Preserve those live grants on upgrade. Multiple
			// holders in one repository, or above an explicit larger limit,
			// cannot arise from that legacy contract and remain a refusal.
			if len(gate.groups[group]) >= limit && (limit != 1 || holder.repoID == run.RepoID) {
				return nil, fmt.Errorf("%w: adopted concurrency group already at limit %d", ErrAdoptionRefused, limit)
			}
		}
	}
	now := time.Now().UTC()
	reservation := &admissionReservation{
		pods: peak,
		gate: gate, sequence: run.QueueSequence, order: order, runID: run.ID, repoID: run.RepoID,
		ref: run.Ref, refKind: run.RefKind, sha: run.SHA,
		size: size.Effective(), weight: gate.weights.of(size.Effective(), gate.capacity),
		group: group, groupName: run.ConcurrencyGroup, groupLimit: max(1, run.ConcurrencyLimit), prepared: true, granted: true, submitted: true,
		claimedAt: now, preparedAt: now, grantedAt: now, ready: make(chan struct{}),
	}
	if gate.workflowOnly || peak != nil {
		reservation.weight = 0
	}
	close(reservation.ready)
	gate.reservations[run.QueueSequence] = reservation
	gate.used += reservation.weight
	gate.runs++
	if group != "" {
		gate.groups[group] = append(gate.groups[group], reservation)
	}
	return reservation, nil
}

func (scheduler *Scheduler) resumeAdoptedRun(ctx context.Context, run model.Run, require mutationRequirement, reservation *admissionReservation) error {
	defer reservation.cancel()
	scheduler.restoreBackfillHistory(ctx, run, reservation)
	if err := require(ctx); err != nil {
		reservation.retain()
		return err
	}
	repository, err := scheduler.database().Repository(ctx, run.RepoID)
	if err != nil {
		reservation.retain()
		return err
	}
	scheduler.admission.mu.Lock()
	reservation.repo = repository.Name
	scheduler.admission.mu.Unlock()
	current, err := scheduler.database().Run(ctx, run.ID)
	if err != nil {
		reservation.retain()
		return err
	}
	if current.Preempted() {
		if err := scheduler.completePendingCancellationsWithGate(ctx, require); err != nil {
			reservation.retain()
			return err
		}
		return nil
	}
	file, events, retained, err := scheduler.logs.Resume(run.ID)
	if err != nil {
		reservation.retain()
		return err
	}
	// A previous owner can stop between Workflow creation and this transition.
	if run.Phase == "waiting" {
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

// Retired execution is terminal, rather than a reason to stop the new owner.
// Delete only the owned Argo Workflow; operators own any external resources.
func (scheduler *Scheduler) failRetiredRun(ctx context.Context, run model.Run, require mutationRequirement) error {
	if err := require(ctx); err != nil {
		return err
	}
	if err := scheduler.jobs.Delete(ctx, run.WorkflowName, run.ID); err != nil {
		return fmt.Errorf("stop retired Workflow %s: %w", run.WorkflowName, err)
	}
	if err := scheduler.finalizeReconciledRun(ctx, run, JobResult{Status: model.RunFailed, Phase: "infrastructure", Error: argoworkflow.ErrWorkflowCapabilityRequired.Error()}, require); err != nil {
		return err
	}
	if err := scheduler.database().CompleteRunCancellation(ctx, run.ID, run.WorkflowName); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	cleanupCtx, cancel := boundedWorkspaceCleanupContext(ctx)
	defer cancel()
	return scheduler.workspaces.cleanupRunDeferred(cleanupCtx, run.ID)
}
