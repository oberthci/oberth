package argojob

import (
	"context"
	"errors"
	"fmt"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/pkg/pipeline"
)

// workflowCreateFailure preserves error classification without public API bodies.
// A failed accepting request is never submitted again in the background.
type workflowCreateFailure struct {
	name, stage string
	cause       error
	uncertain   bool
	infra       bool
}

func (e *workflowCreateFailure) Error() string {
	return fmt.Sprintf("argojob: create Workflow %s failed (stage=%s reason=%s); request another attempt explicitly", e.name, e.stage, executorIdentityErrorClass(e.cause))
}
func (e *workflowCreateFailure) Unwrap() error               { return e.cause }
func (e *workflowCreateFailure) InfrastructureFailure() bool { return e.infra }
func (e *workflowCreateFailure) UncertainCreate() bool       { return e.uncertain }
func transientWorkflowCreate(err error) bool {
	return !errors.Is(err, context.Canceled) && retryExecutorIdentityRead(err)
}
func definiteWorkflowRejection(err error) bool {
	return apierrors.IsInvalid(err) || apierrors.IsForbidden(err) || apierrors.IsBadRequest(err)
}

// createWorkflow posts exactly once. Fixed names make AlreadyExists idempotent;
// cache misses are confirmed by one live GET, with no settlement retry loop.
func (controller *Controller) createWorkflow(ctx context.Context, request Request, intended *wfv1.Workflow) (*wfv1.Workflow, bool, error) {
	if err := controller.verifyCapabilityGrant(ctx, request.RepositoryID, request.Capability); err != nil {
		return nil, true, err
	}
	created, err := controller.workflows.Create(ctx, intended.DeepCopy(), metav1.CreateOptions{})
	if err == nil {
		return created, false, nil
	}
	if definiteWorkflowRejection(err) {
		return nil, true, &workflowCreateFailure{name: intended.Name, stage: "create", cause: err}
	}
	existing, lookupErr := controller.findCreatedWorkflow(ctx, intended.Name, request.RunID)
	if lookupErr == nil {
		if err := controller.sameCreateOwner(existing, request); err != nil {
			return nil, false, err
		}
		return existing, false, nil
	}
	return nil, false, &workflowCreateFailure{name: intended.Name, stage: "create", uncertain: true, cause: errors.Join(err, lookupErr), infra: transientWorkflowCreate(err) || transientWorkflowCreate(lookupErr)}
}

func (controller *Controller) findCreatedWorkflow(ctx context.Context, name, runID string) (*wfv1.Workflow, error) {
	if hub, ok := controller.hub.(interface{ WorkflowsForRun(string) []*wfv1.Workflow }); ok {
		for _, wf := range hub.WorkflowsForRun(runID) {
			if wf.Name == name {
				return wf, nil
			}
		}
	}
	return controller.workflows.Get(ctx, name, metav1.GetOptions{})
}

// Immutable run metadata and the authored execution identity bind a fixed name.
// Argo may default or update native spec fields; adoption does not rebuild them.
func sameRunOwner(existing *wfv1.Workflow, request Request, namespace string) error {
	if existing == nil || existing.Name != request.Name || existing.Namespace != namespace ||
		existing.Labels[runLabel] != labelValue(request.RunID) || existing.Annotations[runIDAnnotation] != request.RunID ||
		existing.Labels["oberth.ci/repo"] != labelValue(request.Repo) || existing.Labels["oberth.ci/sha"] != labelValue(request.SHA) ||
		existing.Annotations[refAnnotation] != request.Ref || existing.Annotations[identityAnnotation] == "" || existing.Labels[triggerLabel] != string(request.Trigger) || existing.Labels["oberth.ci/engine"] != "argo" {
		return fmt.Errorf("argojob: Workflow %s belongs to a different run or owner", request.Name)
	}
	return nil
}

// Issued only after a definitive NotFound and before any Workflow POST. A
// generic create failure or a failed GET cannot make this non-execution claim.
type beforeWorkflowCreateFailure struct{ cause error }

func (e *beforeWorkflowCreateFailure) Error() string {
	return "argojob: prepare before Workflow create: " + e.cause.Error()
}
func (e *beforeWorkflowCreateFailure) Unwrap() error              { return e.cause }
func (e *beforeWorkflowCreateFailure) BeforeWorkflowCreate() bool { return true }
func (e *beforeWorkflowCreateFailure) InfrastructureFailure() bool {
	return retryableWorkflowPreparation(e.cause)
}
func retryableWorkflowPreparation(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !retryableWorkflowPreparation(child) {
				return false
			}
		}
		return true
	}
	if classified, ok := err.(interface{ InfrastructureFailure() bool }); ok {
		return classified.InfrastructureFailure()
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return retryableWorkflowPreparation(wrapped.Unwrap())
	}
	return retryExecutorIdentityRead(err)
}

// WatchFailedCreate stops an owned Workflow if a timed-out POST appears later.
// It consumes the existing hub's events; it never polls, creates, or deletes.
func (controller *Controller) WatchFailedCreate(ctx context.Context, request Request) error {
	ctx, stop := context.WithTimeout(ctx, time.Minute)
	defer stop()
	hub, ok := controller.hub.(interface {
		WorkflowsForRun(string) []*wfv1.Workflow
		Subscribe(string) (<-chan struct{}, func())
	})
	if !ok {
		return errors.New("argojob: failed-create observation requires the Workflow watch hub")
	}
	wake, cancel := hub.Subscribe(request.Name)
	defer cancel()
	for {
		for _, wf := range hub.WorkflowsForRun(request.RunID) {
			if wf.Name != request.Name {
				continue
			}
			if err := sameRunOwner(wf, request, controller.config.Namespace); err != nil {
				return err
			}
			if wf.Status.Phase.Completed() {
				return nil
			}
			if _, err := requestWorkflowShutdown(ctx, controller.workflows, wf, failedCreateShutdown(request)); err == nil {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
		}
	}
}

func (controller *Controller) sameCreateOwner(workflow *wfv1.Workflow, request Request) error {
	if err := sameRunOwner(workflow, request, controller.config.Namespace); err != nil {
		return err
	}
	sources := make([]pipeline.PinnedSource, 0, len(request.PinnedSources))
	for _, tree := range request.PinnedSources {
		sources = append(sources, tree.Source)
	}
	if err := verifyDeliveredPinnedSources(workflow, sources); err != nil {
		return &workflowCreateFailure{name: request.Name, stage: "source-binding", cause: err, uncertain: true}
	}
	return nil
}

func failedCreateShutdown(request Request) wfv1.ShutdownStrategy {
	if request.Trigger == pipeline.TriggerRelease || request.RunTrigger == "tag" || request.RunTrigger == "promotion" || request.RunTrigger == "release" || request.RunTrigger == "apply" {
		return wfv1.ShutdownStrategyTerminate
	}
	return wfv1.ShutdownStrategyStop
}
