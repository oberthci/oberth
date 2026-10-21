package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/vmrunner"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type adoptedRun struct {
	run         model.Run
	reservation *admissionReservation
}

func branchRunAdoptable(run model.Run) bool {
	return run.RefKind == model.RefBranch && !run.Release && run.Trigger != "promotion"
}

func (scheduler *Scheduler) withoutPendingPreflight(ctx context.Context, run model.Run) (bool, error) {
	journal, ok := scheduler.store.(vmrunner.KVMJournal)
	if !ok {
		return true, nil
	}
	pending, err := journal.PendingKVMPreflights(ctx)
	if err != nil {
		return false, fmt.Errorf("load pending preflights before adoption: %w", err)
	}
	for _, execution := range pending {
		if execution.Intent.RunID == run.ID {
			return false, nil
		}
	}
	return true, nil
}

// adoptionFailure names the exact Workflow startup could not adopt, so the
// operator knows which object to delete. A refused Workflow keeps startup
// stopped until it is deleted; recovery then takes the missing-Workflow
// fallback and re-queues the run. A failed read names the same object but
// asks for nothing: the next start retries it.
func adoptionFailure(adopter JobAdopter, run model.Run, err error) error {
	namespace := adopter.JobNamespace()
	failure := fmt.Errorf("adopt run %s: Workflow %s/%s: %w", run.ID, namespace, run.JobName, err)
	if errors.Is(err, ErrAdoptionRefused) {
		failure = fmt.Errorf("%w; startup stops until it is deleted (kubectl -n %s delete workflow %s), after which the run is re-queued",
			failure, namespace, run.JobName)
	}
	log.Printf("ERROR: %v", failure)
	return failure
}

// adopt restores grants for work already running, even if the new owner's
// configured capacity is smaller. New admissions wait until it has drained.
func (gate *weightedAdmission) adopt(run model.Run, size periapsis.Size) (*admissionReservation, error) {
	if !size.Valid() || (run.ConcurrencyGroup != "" && !argoworkflow.ValidConcurrencyGroup(run.ConcurrencyGroup)) {
		return nil, fmt.Errorf("%w: invalid adopted resource contract", ErrAdoptionRefused)
	}
	order := run.AdmissionSequence
	if order <= 0 {
		order = run.QueueSequence
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if previous, ok := gate.reservations[run.QueueSequence]; ok {
		if previous.runID != run.ID || previous.order != order || !previous.granted || previous.size != size.Effective() || previous.groupName != run.ConcurrencyGroup {
			return nil, fmt.Errorf("%w: conflicting adopted admission", ErrAdoptionRefused)
		}
		return previous, nil
	}
	group := ""
	if run.ConcurrencyGroup != "" {
		group = concurrencyGroupKey(run.RepoID, run.ConcurrencyGroup)
		if gate.groups[group] != nil {
			return nil, fmt.Errorf("%w: adopted concurrency group already held", ErrAdoptionRefused)
		}
	}
	now := time.Now().UTC()
	reservation := &admissionReservation{
		gate: gate, sequence: run.QueueSequence, order: order, runID: run.ID, repoID: run.RepoID,
		ref: run.Ref, refKind: run.RefKind, sha: run.SHA,
		size: size.Effective(), weight: gate.weights.of(size.Effective(), gate.capacity),
		group: group, groupName: run.ConcurrencyGroup, prepared: true, granted: true,
		claimedAt: now, preparedAt: now, grantedAt: now, ready: make(chan struct{}),
	}
	close(reservation.ready)
	gate.reservations[run.QueueSequence] = reservation
	gate.used += reservation.weight
	gate.runs++
	if group != "" {
		gate.groups[group] = reservation
	}
	return reservation, nil
}

func (scheduler *Scheduler) resumeAdoptedRun(ctx context.Context, run model.Run, require mutationRequirement, reservation *admissionReservation) error {
	defer reservation.cancel()
	if err := require(ctx); err != nil {
		reservation.retain()
		return err
	}
	repository, err := scheduler.store.Repository(ctx, run.RepoID)
	if err != nil {
		reservation.retain()
		return err
	}
	scheduler.admission.mu.Lock()
	reservation.repo = repository.Name
	scheduler.admission.mu.Unlock()
	current, err := scheduler.store.Run(ctx, run.ID)
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
		run, err = scheduler.store.MarkRunSubmitted(ctx, run.ID)
		if err != nil {
			_ = file.Close()
			reservation.retain()
			return err
		}
	}
	err = scheduler.observeRun(ctx, run, repository, require, reservation, runObservation{
		file: file, events: events, retained: retained, adopted: true,
	})
	cleanupCtx, cancel := boundedWorkspaceCleanupContext(ctx)
	defer cancel()
	current, lookupErr := scheduler.store.Run(cleanupCtx, run.ID)
	if lookupErr != nil || !current.Status.Terminal() {
		return errors.Join(err, lookupErr)
	}
	return errors.Join(err, scheduler.workspaces.cleanupRun(cleanupCtx, run.ID))
}
