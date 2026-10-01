package store

import (
	"context"
	"database/sql"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/vmrunner"
)

func (s *Store) SubmitPilot(ctx context.Context, runID string) error {
	return s.changePilot(ctx, runID, "vm.pilot-submitted", func(tx *sql.Tx, state *pilotState, now time.Time) error {
		value := &state.Execution
		if value.Submitted || value.Cleaning || value.Cleaned || value.Failure != "" || !now.Before(value.CreatedAt.Add(value.Plan.Spec.Deadline)) {
			return ErrInvalidState
		}
		if err := pilotRunEligible(ctx, tx, value.Plan, true); err != nil {
			return err
		}
		value.Submitted = true
		return nil
	})
}

func (s *Store) AddPilotResource(ctx context.Context, runID string, intent vmrunner.ResourceIntent) error {
	return s.changePilot(ctx, runID, "vm.pilot-resource-intent", func(_ *sql.Tx, state *pilotState, now time.Time) error {
		if err := vmrunner.ValidatePilotResource(state.Execution.Plan, intent); err != nil {
			return errors.Join(ErrInvalid, err)
		}
		if current := state.resource(intent.Key); current != nil {
			if current.Intent != intent {
				return ErrInvalidState
			}
			return nil
		}
		// Automatically created Pods are journalled by ObservePilotPod, which
		// also works during cleanup. This method only authorizes host creates.
		if intent.Kind == "Pod" || !state.creating(now) || len(state.Resources) >= vmrunner.MaxPilotResources {
			return ErrInvalidState
		}
		state.Resources = append(state.Resources, vmrunner.PilotResource{Intent: intent})
		return nil
	})
}

func (s *Store) SubmitPilotResource(ctx context.Context, runID, key string) error {
	return s.changePilot(ctx, runID, "vm.pilot-resource-submitted", func(tx *sql.Tx, state *pilotState, now time.Time) error {
		resource := state.resource(key)
		if !state.creating(now) || resource == nil || resource.Intent.Kind == "Pod" || resource.Submitted || resource.Cleaning || resource.Cleaned {
			return ErrInvalidState
		}
		if err := pilotRunEligible(ctx, tx, state.Execution.Plan, true); err != nil {
			return err
		}
		if key == vmrunner.RestartVMResource {
			previous := state.resource(vmrunner.InitialVMResource)
			if state.Execution.Restart == nil || state.Execution.Restart.Response != nil || previous == nil || !previous.Cleaned || previous.Receipt.UID == "" || !pilotChildrenCleaned(state, vmrunner.InitialVMResource) {
				return ErrInvalidState
			}
		}
		resource.Submitted = true
		return nil
	})
}

func validPilotResourceReceipt(state *pilotState, intent vmrunner.ResourceIntent, receipt vmrunner.ResourceReceipt, now time.Time) bool {
	if len(receipt.UID) < 1 || len(receipt.UID) > 128 || strings.TrimSpace(receipt.UID) != receipt.UID || strings.ContainsAny(receipt.UID, "\x00\r\n\t") || receipt.SpecIdentity != intent.SpecIdentity || receipt.CreatedAt.IsZero() || receipt.CreatedAt.After(now) {
		return false
	}
	if intent.Parent == "" {
		return receipt.OwnerUID == "" && receipt.PodIP == ""
	}
	if receipt.PodIP != "" {
		address, err := netip.ParseAddr(receipt.PodIP)
		if err != nil || address.IsUnspecified() || address.IsMulticast() || address.Zone() != "" || address.String() != receipt.PodIP {
			return false
		}
	}
	parent := state.resource(intent.Parent)
	return parent != nil && parent.Submitted && parent.Receipt.UID != "" && receipt.OwnerUID == parent.Receipt.UID && parent.Intent.Namespace == intent.Namespace
}

func (s *Store) BindPilotResource(ctx context.Context, runID, key string, receipt vmrunner.ResourceReceipt) error {
	return s.changePilot(ctx, runID, "vm.pilot-resource-bound", func(_ *sql.Tx, state *pilotState, now time.Time) error {
		resource := state.resource(key)
		if resource == nil || !resource.Submitted || resource.Rejected || resource.Cleaned || state.Execution.Cleaned || resource.Intent.Kind == "Pod" || !validPilotResourceReceipt(state, resource.Intent, receipt, now) {
			return ErrInvalidState
		}
		if resource.Receipt.UID != "" {
			if !samePilotResourceReceipt(resource.Receipt, receipt) {
				return poisonPilot(state, "resource-replaced")
			}
			return nil
		}
		for _, owned := range state.Resources {
			if owned.Receipt.UID == receipt.UID {
				return poisonPilot(state, "resource-replaced")
			}
		}
		// An uncertain create can return after cancellation. Binding its exact
		// UID is still required so recovery can remove that owned object.
		resource.Receipt = receipt
		return nil
	})
}

// ObservePilotPod records API-observed child ownership, never a label-based
// adoption or authority to create a Pod. Recovery may find a late owned child
// while the parent is being deleted; it must retain that obligation too.
func (s *Store) ObservePilotPod(ctx context.Context, runID string, intent vmrunner.ResourceIntent, receipt vmrunner.ResourceReceipt) error {
	return s.changePilot(ctx, runID, "vm.pilot-child-observed", func(_ *sql.Tx, state *pilotState, now time.Time) error {
		if vmrunner.ValidatePilotResource(state.Execution.Plan, intent) != nil || intent.Kind != "Pod" || !state.Execution.Submitted || state.Execution.Cleaned || !validPilotResourceReceipt(state, intent, receipt, now) {
			return ErrInvalidState
		}
		parent := state.resource(intent.Parent)
		if current := state.resource(intent.Key); current != nil {
			if current.Intent != intent || !samePilotResourceReceipt(current.Receipt, receipt) {
				return poisonPilot(state, "resource-replaced")
			}
			return nil
		}
		if len(state.Resources) >= vmrunner.MaxPilotResources {
			return poisonPilot(state, "resource-limit")
		}
		for _, owned := range state.Resources {
			if owned.Receipt.UID == receipt.UID {
				return poisonPilot(state, "resource-replaced")
			}
		}
		unexpected := parent.Cleaned
		// A newly observed owned child invalidates an earlier parent cleanup
		// observation. Retain both obligations until recovery observes them gone.
		if parent.Cleaned {
			parent.Cleaned = false
		}
		for _, owned := range state.Resources {
			if owned.Intent.Parent == intent.Parent {
				unexpected = true
			}
		}
		state.Resources = append(state.Resources, vmrunner.PilotResource{Intent: intent, Receipt: receipt, Submitted: true, Cleaning: state.Execution.Cleaning})
		if unexpected {
			return poisonPilot(state, "attempt-replaced")
		}
		return nil
	})
}

func (s *Store) RejectPilotResource(ctx context.Context, runID, key string) error {
	return s.changePilot(ctx, runID, "vm.pilot-resource-refused", func(_ *sql.Tx, state *pilotState, _ time.Time) error {
		resource := state.resource(key)
		if resource == nil || !resource.Submitted || resource.Cleaned || resource.Receipt.UID != "" || resource.Intent.Kind == "Pod" {
			return ErrInvalidState
		}
		resource.Rejected = true
		return nil
	})
}

func (s *Store) BeginPilotCleanup(ctx context.Context, runID string) error {
	return s.changePilot(ctx, runID, "vm.pilot-cleanup-required", func(_ *sql.Tx, state *pilotState, _ time.Time) error {
		state.Execution.Cleaning = true
		return nil
	})
}

func (s *Store) BeginPilotResourceCleanup(ctx context.Context, runID, key string) error {
	return s.changePilot(ctx, runID, "vm.pilot-resource-cleanup-required", func(_ *sql.Tx, state *pilotState, _ time.Time) error {
		resource := state.resource(key)
		if resource == nil {
			return ErrNotFound
		}
		resource.Cleaning = true
		return nil
	})
}

func pilotChildrenCleaned(state *pilotState, parent string) bool {
	for _, child := range state.Resources {
		if child.Intent.Parent == parent && !child.Cleaned {
			return false
		}
	}
	return true
}

// Completion means the host has observed absence, including all owned children;
// a successful delete request or a same-name replacement cannot supply it.
func (s *Store) CompletePilotResourceCleanup(ctx context.Context, runID, key string, receipt vmrunner.ResourceReceipt) error {
	return s.changePilot(ctx, runID, "vm.pilot-resource-cleanup-observed", func(_ *sql.Tx, state *pilotState, _ time.Time) error {
		resource := state.resource(key)
		if resource == nil || !resource.Cleaning || !samePilotResourceReceipt(resource.Receipt, receipt) || !pilotChildrenCleaned(state, key) {
			return ErrInvalidState
		}
		if resource.Submitted && !resource.Rejected && resource.Receipt.UID == "" {
			return ErrInvalidState
		}
		resource.Cleaned = true
		return nil
	})
}

func (s *Store) CompletePilotCleanup(ctx context.Context, runID string) error {
	return s.changePilot(ctx, runID, "vm.pilot-cleanup-observed", func(tx *sql.Tx, state *pilotState, _ time.Time) error {
		if !state.Execution.Cleaning {
			return ErrInvalidState
		}
		if state.Execution.Cleaned {
			return nil
		}
		for _, resource := range state.Resources {
			if !resource.Cleaned {
				return ErrInvalidState
			}
		}
		if err := releaseVMCapacity(ctx, tx, runID); err != nil {
			return err
		}
		state.Execution.Cleaned = true
		return nil
	})
}

func samePilotResourceReceipt(a, b vmrunner.ResourceReceipt) bool {
	return a.UID == b.UID && a.SpecIdentity == b.SpecIdentity && a.OwnerUID == b.OwnerUID && a.PodIP == b.PodIP && a.CreatedAt.Equal(b.CreatedAt)
}
