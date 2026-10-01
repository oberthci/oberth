package store

import (
	"context"
	"database/sql"
	"net/netip"
	"time"

	"github.com/oberthci/oberth/internal/vmrunner"
)

func pilotEndpointBound(state *pilotState, endpoint vmrunner.PilotEndpoint, active bool) bool {
	if vmrunner.ValidatePilotEndpoint(endpoint) != nil {
		return false
	}
	address, err := netip.ParseAddrPort(endpoint.Address)
	if err != nil || address.Port() != 443 {
		return false
	}
	instance := state.resource(endpoint.Attempt)
	if instance == nil || instance.Receipt.UID != endpoint.VMIUID || instance.Rejected || !instance.Submitted || (active && (instance.Cleaning || instance.Cleaned)) {
		return false
	}
	for _, pod := range state.Resources {
		if pod.Intent.Parent == endpoint.Attempt && pod.Receipt.UID == endpoint.LauncherUID && pod.Receipt.OwnerUID == endpoint.VMIUID && pod.Receipt.PodIP == address.Addr().String() && (!active || (!pod.Cleaning && !pod.Cleaned)) {
			return true
		}
	}
	return false
}

func (s *Store) BindPilotEndpoint(ctx context.Context, runID string, endpoint vmrunner.PilotEndpoint) error {
	return s.changePilot(ctx, runID, "vm.pilot-endpoint-bound", func(_ *sql.Tx, state *pilotState, now time.Time) error {
		if endpoint.Attempt != vmrunner.InitialVMResource || !pilotEndpointBound(state, endpoint, true) {
			return ErrInvalidState
		}
		if current := state.Execution.Endpoint; current != nil {
			if *current != endpoint {
				return poisonPilot(state, "resource-replaced")
			}
			return nil
		}
		if !state.creating(now) {
			return ErrInvalidState
		}
		state.Execution.Endpoint = &endpoint
		return nil
	})
}

// ClaimPilotRestart grants exactly one caller permission to perform the finite
// restart. A duplicate delivery returns claimed=false; it may read the already
// completed durable reply, but may never recreate or restart resources again.
func (s *Store) ClaimPilotRestart(ctx context.Context, runID string, request vmrunner.PilotRestartRequest) (bool, error) {
	claimed := false
	err := s.changePilot(ctx, runID, "vm.pilot-restart-claimed", func(tx *sql.Tx, state *pilotState, now time.Time) error {
		value := &state.Execution
		if value.Attempt == nil || value.Endpoint == nil || vmrunner.ValidatePilotRestartRequest(value.Plan, *value.Attempt, request) != nil || request.Old != *value.Endpoint {
			return ErrInvalidState
		}
		if value.Restart != nil {
			if vmrunner.PilotRestartIdentity(value.Restart.Request) != vmrunner.PilotRestartIdentity(request) {
				return poisonPilot(state, "invalid-receipt")
			}
			return nil
		}
		if !state.creating(now) || !pilotEndpointBound(state, request.Old, true) {
			return ErrInvalidState
		}
		if err := pilotRunEligible(ctx, tx, value.Plan, true); err != nil {
			return err
		}
		value.Restart = &vmrunner.PilotRestart{Request: request}
		claimed = true
		return nil
	})
	return claimed && err == nil, err
}

func (s *Store) CompletePilotRestart(ctx context.Context, runID string, response vmrunner.PilotRestartResponse) error {
	return s.changePilot(ctx, runID, "vm.pilot-restart-completed", func(tx *sql.Tx, state *pilotState, now time.Time) error {
		value := &state.Execution
		if value.Restart == nil {
			return ErrInvalidState
		}
		if vmrunner.ValidatePilotRestartResponse(value.Restart.Request, response) != nil {
			return poisonPilot(state, "invalid-receipt")
		}
		if current := value.Restart.Response; current != nil {
			if *current != response {
				return poisonPilot(state, "invalid-receipt")
			}
			return nil
		}
		previous := state.resource(vmrunner.InitialVMResource)
		if !state.creating(now) || previous == nil || !previous.Cleaned || !pilotChildrenCleaned(state, vmrunner.InitialVMResource) || !pilotEndpointBound(state, response.Endpoint, true) {
			return ErrInvalidState
		}
		if err := pilotRunEligible(ctx, tx, value.Plan, true); err != nil {
			return err
		}
		value.Restart.Response = &response
		return nil
	})
}
