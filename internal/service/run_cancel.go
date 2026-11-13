package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// RunCancelStore records a Stop request, its audit action and
// Workflow-Stop obligation in one transaction.
type RunCancelStore interface {
	CancelRun(context.Context, model.RunCancelRequest) (model.RunCancelResult, error)
}

// RunCancellationObserver executes the obligations a cancel recorded: it
// withdraws the run's admission reservation and stops its Workflow. The
// scheduler implements it; an API without one leaves the obligations to the
// scheduler's next cancellation pass.
type RunCancellationObserver interface {
	AcceptCancellations(context.Context, []model.RunCancellation) error
}

// Workflow states reported by run_cancel.
const (
	cancelWorkflowNone    = "none"          // never claimed: no Workflow can exist
	cancelWorkflowNotYet  = "not_submitted" // claimed, no Workflow created
	cancelWorkflowPending = "pending"       // obligation recorded; the scheduler retries Stop
)

// releaseLikeRun reports runs whose Workflow may publish or mutate outside
// Oberth (tag/release, trusted apply): cancelling one mid-flight can leave that
// publication partial.
func releaseLikeRun(run model.Run) bool {
	return run.Release || run.RefKind == model.RefTag || run.Trigger == "tag" || run.Trigger == "release" || run.Trigger == "apply"
}

// RunCancelPolicy decides whether an actor may cancel run and returns the
// warning to record with the cancel.
//
// A non-admin uplink may cancel only ordinary branch-push runs (trigger
// branch): release, tag, plan, apply, promotion and scheduled runs are
// refused. Which
// repositories it can see is the caller's scope check. An admin may cancel any
// run, but a tag/release or apply run requires an explicit force, because its
// Workflow may already have published part of its output.
func RunCancelPolicy(run model.Run, admin, force bool) (string, error) {
	releaseLike := releaseLikeRun(run)
	if !admin {
		switch {
		case releaseLike:
			return "", refuse(ErrForbidden, "release, tag and apply runs can be cancelled only by an admin uplink")
		case run.RefKind != model.RefBranch || run.Trigger != "branch":
			return "", refuse(ErrForbidden, "a non-admin uplink may cancel only branch-push runs, not %s runs", run.Trigger)
		}
		return "", nil
	}
	var warnings []string
	if releaseLike {
		if !force {
			return "", fmt.Errorf("%w: run %s is a %s run; cancelling it can leave its publication partial "+
				"(anything its Workflow already pushed stays published). Repeat with force to cancel it anyway", ErrInvalidInput, run.ID, run.Trigger)
		}
		warnings = append(warnings, "publication may be partial: anything the Workflow already pushed stays published, and Oberth publishes nothing further for this run")
	}
	return strings.Join(warnings, "; "), nil
}

// CancelRun is the audited cancel of one queued, waiting or running run by
// actor. It resolves the run within the actor's scope, applies RunCancelPolicy,
// records the request durably, and hands any Workflow-Stop obligation to
// the scheduler, which also cancels preparation and retains its worker custody
// until join. Repeating the same Stop is idempotent.
func (service *API) CancelRun(ctx context.Context, actor api.Actor, runID, reason string, force bool) (api.RunCancelResponse, error) {
	if strings.TrimSpace(actor.Identity) == "" {
		return api.RunCancelResponse{}, errors.New("service: acting uplink identity is required")
	}
	scoped, err := service.forActor(ctx, actor)
	if err != nil {
		return api.RunCancelResponse{}, err
	}
	if err := scoped.requireMutation(ctx); err != nil {
		return api.RunCancelResponse{}, err
	}
	return scoped.cancelRun(ctx, actor, runID, reason, force)
}

func (service *API) cancelRun(ctx context.Context, actor api.Actor, id, reason string, force bool) (api.RunCancelResponse, error) {
	if strings.TrimSpace(id) == "" {
		return api.RunCancelResponse{}, fmt.Errorf("%w: run ID is required", ErrInvalidInput)
	}
	if service.history == nil || service.runCancels == nil {
		return api.RunCancelResponse{}, refuse(ErrUnavailable, "run cancel")
	}
	reason = strings.TrimSpace(reason)
	if err := store.ValidateCancelReason(reason); err != nil {
		return api.RunCancelResponse{}, fmt.Errorf("%w: %s", ErrInvalidInput, strings.TrimPrefix(err.Error(), store.ErrInvalid.Error()+": "))
	}
	run, err := service.resolveReadRunID(ctx, id)
	if err != nil {
		return api.RunCancelResponse{}, err
	}
	if _, err := RunCancelPolicy(run, actor.Admin, force); err != nil {
		return api.RunCancelResponse{}, err
	}
	// The store repeats the decision on the run as it is inside the cancel
	// transaction: the run may have changed since.
	var warning string
	result, err := service.runCancels.CancelRun(ctx, model.RunCancelRequest{
		RunID: run.ID, Actor: actor.Identity, Reason: reason, Force: force,
		Authorize: func(current model.Run) (string, error) {
			decided, err := RunCancelPolicy(current, actor.Admin, force)
			warning = decided
			return decided, err
		},
	})
	if err != nil {
		return api.RunCancelResponse{}, err
	}
	if result.Run.Status.Terminal() {
		forgetActiveLogIndex(service.logs, run.ID)
	}
	service.signals.NotifyRun(run.ID)
	if result.PromotionID != "" {
		service.signals.NotifyPromotion(result.PromotionID)
	}
	response := api.RunCancelResponse{
		RunID: result.Run.ID, Status: string(result.Run.Status), Phase: result.Run.Phase,
		PreviousState: string(result.PreviousState), CancelledBy: actor.Identity, Reason: result.Run.Reason,
		Workflow: cancelWorkflowNone, PromotionID: result.PromotionID, Warning: warning,
	}
	if len(result.Cancellations) == 0 {
		return response, nil
	}
	response.Workflow = cancelWorkflowPending
	if result.Cancellations[0].WorkflowName == "" {
		response.Workflow = cancelWorkflowNotYet
	}
	if observer, ok := service.enqueues.(RunCancellationObserver); ok {
		if err := observer.AcceptCancellations(ctx, result.Cancellations); err != nil {
			// The cancel is durable; the scheduler's cancellation pass retries
			// Stop. Report it instead of failing a committed cancel.
			log.Printf("run cancel %s: Workflow Stop deferred to the cancellation pass: %v", run.ID, err)
			response.Workflow = cancelWorkflowPending
		}
	}
	return response, nil
}
