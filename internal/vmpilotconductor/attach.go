package vmpilotconductor

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/oberthci/oberth/internal/vmnodebridge"
	"github.com/oberthci/oberth/internal/vmpilotfixture"
	"github.com/oberthci/oberth/internal/vmrunner"
)

// RunBoundStream is the only capability-delivery path for the inactive pilot.
// It records the one-use claim before opening a node channel, verifies the
// conductor's first in-stream proof, preserves the runtime's precise start,
// then lends the fixture's sole bootstrap frame. It does not publish a result;
// the caller must still observe exact termination and UID cleanup.
func (backend *Backend) RunBoundStream(ctx context.Context, plan vmrunner.PilotPlan, node *vmnodebridge.Client,
	fixture *vmpilotfixture.Fixture, endpoint vmrunner.PilotEndpoint, restart vmrunner.PilotRestartHandler) (results vmrunner.ConductorResults, resultErr error) {
	if backend == nil || node == nil || fixture == nil || restart == nil {
		return vmrunner.ConductorResults{}, errors.New("conductor: bound stream prerequisites unavailable")
	}
	attempt, err := backend.BindAttempt(ctx, plan)
	if err != nil {
		return vmrunner.ConductorResults{}, err
	}
	claim, granted, err := backend.journal.ClaimConductorAttach(ctx, plan.Spec.RunID, attempt)
	if err != nil || !granted {
		return vmrunner.ConductorResults{}, errors.New("conductor: attach claim unavailable")
	}
	defer func() {
		if resultErr != nil {
			failCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			resultErr = errors.Join(resultErr, backend.journal.FailPilot(failCtx, plan.Spec.RunID, "runtime"))
		}
	}()
	execution, err := backend.journal.PilotExecution(ctx, plan.Spec.RunID)
	if err != nil || execution.Attach == nil || *execution.Attach != claim || execution.Attempt == nil ||
		vmrunner.ConductorAttemptIdentity(*execution.Attempt) != vmrunner.ConductorAttemptIdentity(attempt) {
		return vmrunner.ConductorResults{}, errors.New("conductor: attach journal changed")
	}
	request := vmnodebridge.AttachRequest{Plan: plan, Attempt: attempt, Claim: claim, CreatedAt: execution.CreatedAt}
	stream, err := node.Open(ctx, request)
	if err != nil {
		return vmrunner.ConductorResults{}, err
	}
	defer func() { _ = stream.Close() }()
	proofCtx, cancelProof := context.WithTimeout(ctx, 15*time.Second)
	boundReader, err := vmrunner.ProveConductorStream(proofCtx, stream.Output, stream.Input, plan, attempt, claim)
	cancelProof()
	if err != nil {
		return vmrunner.ConductorResults{}, err
	}
	if err := backend.journal.BindConductorAttach(ctx, plan.Spec.RunID, claim.Challenge, stream.RuntimeStartedAt); err != nil {
		return vmrunner.ConductorResults{}, err
	}
	if err := backend.journal.BindPilotEndpoint(ctx, plan.Spec.RunID, endpoint); err != nil {
		return vmrunner.ConductorResults{}, err
	}
	if err := fixture.WithBootstrap(attempt, endpoint, func(frame []byte) error {
		if n, err := stream.Input.Write(frame); err != nil || n != len(frame) {
			return io.ErrShortWrite
		}
		return nil
	}); err != nil {
		return vmrunner.ConductorResults{}, err
	}
	return vmrunner.ReadConductorStream(ctx, boundReader, stream.Input, plan, attempt, restart)
}
