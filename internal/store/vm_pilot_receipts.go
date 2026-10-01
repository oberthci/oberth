package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/oberthci/oberth/internal/vmrunner"
)

func (s *Store) BindConductorAttempt(ctx context.Context, runID string, attempt vmrunner.ConductorAttempt) error {
	return s.changePilot(ctx, runID, "vm.pilot-attempt-bound", func(_ *sql.Tx, state *pilotState, now time.Time) error {
		if current := state.Execution.Attempt; current != nil {
			if vmrunner.ConductorAttemptIdentity(*current) != vmrunner.ConductorAttemptIdentity(attempt) {
				return poisonPilot(state, "attempt-replaced")
			}
			return nil
		}
		if vmrunner.ValidateConductorAttempt(state.Execution.Plan, attempt) != nil {
			return ErrInvalid
		}
		// Kubernetes metav1.Time reports whole seconds, while durable admission
		// keeps nanoseconds. A coarse observation cannot order events within
		// that admission second. Preserve the actual observed StartedAt; only
		// its lower-bound comparison uses the matching precision. Precise
		// observations, current time and deadline checks remain unchanged.
		earliestStart := state.Execution.CreatedAt
		if attempt.StartedAt.Nanosecond() == 0 {
			earliestStart = earliestStart.Truncate(time.Second)
		}
		if !state.creating(now) || attempt.StartedAt.Before(earliestStart) || attempt.StartedAt.After(now) {
			return ErrInvalidState
		}
		job := state.resource(vmrunner.ConductorResource)
		if job == nil || job.Receipt.UID != attempt.JobUID || job.Cleaning || job.Cleaned {
			return ErrInvalidState
		}
		pods := 0
		matched := false
		for _, resource := range state.Resources {
			if resource.Intent.Parent != vmrunner.ConductorResource {
				continue
			}
			pods++
			if !resource.Cleaning && !resource.Cleaned && resource.Receipt.UID == attempt.PodUID && resource.Receipt.OwnerUID == attempt.JobUID && resource.Intent.SpecIdentity == attempt.SpecIdentity {
				matched = true
			}
		}
		if pods != 1 || !matched {
			return poisonPilot(state, "attempt-replaced")
		}
		state.Execution.Attempt = &attempt
		return nil
	})
}

func (s *Store) RecordPilotReceipt(ctx context.Context, runID string, receipt vmrunner.PilotReceipt) error {
	return s.changePilot(ctx, runID, "vm.pilot-receipt", func(tx *sql.Tx, state *pilotState, now time.Time) error {
		value := &state.Execution
		if value.Failure != "" || value.Cleaned || value.Attempt == nil || value.Attach == nil || value.Attach.BoundAt.IsZero() {
			return ErrInvalidState
		}
		if vmrunner.VerifyPilotReceipt(value.Plan, receipt) != nil || vmrunner.ConductorAttemptIdentity(*value.Attempt) != vmrunner.ConductorAttemptIdentity(receipt.Attempt) || receipt.Termination.FinishedAt.After(now) || receipt.Termination.FinishedAt.After(value.CreatedAt.Add(value.Plan.Spec.Deadline)) {
			return poisonPilot(state, "invalid-receipt")
		}
		if err := pilotRunEligible(ctx, tx, value.Plan, true); err != nil {
			return err
		}
		if value.Receipt != nil {
			before, err := json.Marshal(value.Receipt)
			if err != nil {
				return err
			}
			after, err := json.Marshal(receipt)
			if err != nil {
				return err
			}
			if string(before) != string(after) {
				return poisonPilot(state, "invalid-receipt")
			}
			return nil
		}
		value.Receipt = &receipt
		return nil
	})
}

// Failure codes are fixed diagnostics, not arbitrary guest/runtime strings that
// might contain fixture keys or candidate-controlled logs.
func (s *Store) FailPilot(ctx context.Context, runID, code string) error {
	switch code {
	case "canceled", "deadline", "runtime", "artifact-changed", "cleanup-failed", "attempt-replaced", "resource-replaced", "resource-limit", "invalid-receipt":
	default:
		return ErrInvalid
	}
	err := s.changePilot(ctx, runID, "vm.pilot-failed", func(_ *sql.Tx, state *pilotState, _ time.Time) error {
		return poisonPilot(state, code)
	})
	var rejected pilotRejection
	if errors.As(err, &rejected) {
		return nil
	}
	return err
}

// CompletedPilotReceipt reads the receipt and every cleanup obligation in the
// same transaction. It is a prerequisite for publication, not an enabled
// scheduler provider. The caller must also compare its current policy seal.
func (s *Store) CompletedPilotReceipt(ctx context.Context, runID string) (vmrunner.PilotReceipt, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return vmrunner.PilotReceipt{}, err
	}
	defer func() { _ = tx.Rollback() }()
	state, err := readPilotState(ctx, tx, runID)
	if err != nil {
		return vmrunner.PilotReceipt{}, err
	}
	value := state.Execution
	if !value.Submitted || !value.Cleaned || value.Failure != "" || value.Attempt == nil || value.Attach == nil || value.Attach.BoundAt.IsZero() || value.Receipt == nil {
		return vmrunner.PilotReceipt{}, ErrInvalidState
	}
	if value.Endpoint == nil || value.Restart == nil || value.Restart.Response == nil ||
		!pilotEndpointBound(&state, *value.Endpoint, false) || !pilotEndpointBound(&state, value.Restart.Response.Endpoint, false) {
		return vmrunner.PilotReceipt{}, ErrInvalidState
	}
	if err := pilotRunEligible(ctx, tx, value.Plan, false); err != nil {
		return vmrunner.PilotReceipt{}, err
	}
	if err := vmrunner.VerifyPilotReceipt(value.Plan, *value.Receipt); err != nil {
		return vmrunner.PilotReceipt{}, errors.Join(ErrInvalidState, err)
	}
	if value.Receipt.Termination.FinishedAt.After(value.CreatedAt.Add(value.Plan.Spec.Deadline)) {
		return vmrunner.PilotReceipt{}, ErrInvalidState
	}
	for _, resource := range state.Resources {
		if !resource.Cleaned || !resource.Submitted || resource.Rejected || resource.Receipt.UID == "" {
			return vmrunner.PilotReceipt{}, ErrInvalidState
		}
	}
	// A stream's restart case cannot replace observed ownership of both VMs.
	for _, key := range []string{vmrunner.InitialVMResource, vmrunner.RestartVMResource, vmrunner.ConductorResource, "beacon-endpoint", "guest-isolation", "conductor-isolation"} {
		if state.resource(key) == nil {
			return vmrunner.PilotReceipt{}, ErrInvalidState
		}
	}
	for _, key := range []string{vmrunner.InitialVMResource, vmrunner.RestartVMResource, vmrunner.ConductorResource} {
		parent := state.resource(key)
		children := 0
		for _, child := range state.Resources {
			if child.Intent.Parent != key {
				continue
			}
			children++
			if child.Receipt.OwnerUID != parent.Receipt.UID {
				return vmrunner.PilotReceipt{}, ErrInvalidState
			}
			if key == vmrunner.ConductorResource && (child.Receipt.UID != value.Attempt.PodUID || parent.Receipt.UID != value.Attempt.JobUID || child.Intent.SpecIdentity != value.Attempt.SpecIdentity) {
				return vmrunner.PilotReceipt{}, ErrInvalidState
			}
		}
		if children != 1 {
			return vmrunner.PilotReceipt{}, ErrInvalidState
		}
	}
	return *value.Receipt, nil
}
