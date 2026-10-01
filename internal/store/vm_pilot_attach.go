package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"time"

	"github.com/oberthci/oberth/internal/vmrunner"
)

// ClaimConductorAttach spends the sole attach right before any channel is
// opened. Returning claimed=false never permits another attach, even when the
// first call failed after commit and the caller lost its response.
func (s *Store) ClaimConductorAttach(ctx context.Context, runID string, attempt vmrunner.ConductorAttempt) (vmrunner.ConductorAttachClaim, bool, error) {
	var claim vmrunner.ConductorAttachClaim
	claimed := false
	err := s.changePilot(ctx, runID, "vm.pilot-attach-claimed", func(tx *sql.Tx, state *pilotState, now time.Time) error {
		value := &state.Execution
		if value.Attempt == nil || vmrunner.ConductorAttemptIdentity(*value.Attempt) != vmrunner.ConductorAttemptIdentity(attempt) {
			return ErrInvalidState
		}
		if value.Attach != nil {
			return nil
		}
		if !state.creating(now) || attempt.PodName == "" || attempt.NodeName == "" {
			return ErrInvalidState
		}
		job := state.resource(vmrunner.ConductorResource)
		if job == nil || job.Receipt.UID != attempt.JobUID || job.Cleaning || job.Cleaned {
			return ErrInvalidState
		}
		pods := 0
		for _, resource := range state.Resources {
			if resource.Intent.Parent != vmrunner.ConductorResource {
				continue
			}
			pods++
			if resource.Intent.Name != attempt.PodName || resource.Receipt.UID != attempt.PodUID || resource.Receipt.OwnerUID != attempt.JobUID || resource.Intent.SpecIdentity != attempt.SpecIdentity || resource.Cleaning || resource.Cleaned {
				return ErrInvalidState
			}
		}
		if pods != 1 || pilotRunEligible(ctx, tx, value.Plan, true) != nil {
			return ErrInvalidState
		}
		var nonce [32]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return err
		}
		claim = vmrunner.ConductorAttachClaim{AttemptIdentity: vmrunner.ConductorAttemptIdentity(attempt), Challenge: hex.EncodeToString(nonce[:]), ClaimedAt: now}
		if err := vmrunner.ValidateConductorAttachClaim(value.Plan, attempt, claim); err != nil {
			return ErrInvalid
		}
		value.Attach = &claim
		claimed = true
		return nil
	})
	return claim, claimed && err == nil, err
}

// BindConductorAttach records the runtime's nanosecond start only after the
// protected stream has answered the journaled challenge. It cannot unspend the
// attach right or authorize another channel.
func (s *Store) BindConductorAttach(ctx context.Context, runID, challenge string, runtimeStartedAt time.Time) error {
	return s.changePilot(ctx, runID, "vm.pilot-attach-bound", func(_ *sql.Tx, state *pilotState, now time.Time) error {
		value := &state.Execution
		if value.Attempt == nil || value.Attach == nil || value.Attach.Challenge != challenge || !state.creating(now) {
			return ErrInvalidState
		}
		if !value.Attach.BoundAt.IsZero() {
			if value.Attach.RuntimeStartedAt.Equal(runtimeStartedAt) {
				return nil
			}
			return poisonPilot(state, "attempt-replaced")
		}
		bound := *value.Attach
		bound.RuntimeStartedAt = runtimeStartedAt
		bound.BoundAt = now
		if vmrunner.ValidateConductorAttachClaim(value.Plan, *value.Attempt, bound) != nil || runtimeStartedAt.Before(value.CreatedAt) || !runtimeStartedAt.Before(value.CreatedAt.Add(value.Plan.Spec.Deadline)) {
			return poisonPilot(state, "attempt-replaced")
		}
		value.Attach = &bound
		return nil
	})
}
