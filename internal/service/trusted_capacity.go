package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/vmrunner"
)

const trustedCapacityRetryInterval = 5 * time.Second

// executeTrustedWithCapacity retries only a refused reservation, never a
// submitted or otherwise uncertain execution. Execute constructs fresh absolute
// admission/deadline times after each retry; an unsuccessful transaction has not
// journaled an intent, started a Workflow, or delivered credentials.
func (scheduler *Scheduler) executeTrustedWithCapacity(ctx context.Context, job JobRequest, requirement vmrunner.Requirement, output io.Writer, reservation *admissionReservation, require mutationRequirement) (bool, error) {
	for {
		err := scheduler.trusted.Execute(ctx, job, requirement, output)
		if !errors.Is(err, store.ErrVMCapacity) {
			return false, err
		}
		journal := scheduler.store.(vmrunner.KVMJournal)
		if _, lookupErr := journal.KVMPreflight(ctx, job.Run.ID); !errors.Is(lookupErr, store.ErrNotFound) {
			return false, errors.Join(err, lookupErr)
		}
		if err := reservation.parkVM(); err != nil {
			return false, err
		}
		if _, err := scheduler.store.MarkRunParked(ctx, job.Run.ID); err != nil {
			return false, err
		}
		if err := scheduler.capacityWait(ctx); err != nil {
			return false, err
		}

		current, err := scheduler.store.Run(ctx, job.Run.ID)
		if err != nil {
			return false, err
		}
		if current.Status != model.RunRunning || current.SupersededBy != "" {
			return true, nil
		}
		if err := require(ctx); err != nil {
			return false, err
		}

		scheduler.admissionRequested(ctx, job.Run.ID)
		_, err = reservation.acquireRequest(ctx, admissionRequest{RunID: job.Run.ID, RepoID: job.Repository.ID, Size: job.Size, Group: job.ConcurrencyGroup})
		if errors.Is(err, errAdmissionAbandoned) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		// Reacquiring resources can itself wait: check cancellation and the mutation
		// gate again before any external preflight work.
		current, err = scheduler.store.Run(ctx, job.Run.ID)
		if err != nil {
			return false, err
		}
		if current.Status != model.RunRunning || current.SupersededBy != "" {
			return true, nil
		}
		if err := require(ctx); err != nil {
			return false, err
		}
		readmitted, err := scheduler.store.MarkRunAdmitted(ctx, job.Run.ID)
		if err != nil {
			return false, err
		}
		if readmitted.Preempted() {
			return true, nil
		}
		// Reverify live policy and audit only a changed decision or pin pair.
		fresh, err := scheduler.trustedRequirement(ctx, job.Repository, job.Run.ID, requirement.CandidateSHA)
		if err != nil {
			return false, err
		}
		if vmrunner.ContractDigest(fresh.Contract) != vmrunner.ContractDigest(requirement.Contract) {
			return false, fmt.Errorf("trusted test policy changed while waiting for VM capacity")
		}
		requirement = fresh
	}
}

func waitTrustedCapacity(ctx context.Context) error {
	timer := time.NewTimer(trustedCapacityRetryInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
