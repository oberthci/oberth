package vmrunner

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// PilotResourceObservation is produced by the trusted host adapter after
// checking actual API object fields. Absent refers to the exact recorded UID;
// a replacement name/spec is an error, never an adopted observation.
type PilotResourceObservation struct {
	Receipt ResourceReceipt
	Absent  bool
	Pods    []PilotOwnedPod
}

type PilotOwnedPod struct {
	Intent  ResourceIntent
	Receipt ResourceReceipt
}

// PilotCleanupRuntime has no guest/conductor command, URL or namespace input.
// Observe must list actual owner-UID children even after a parent is absent.
// Delete uses a UID precondition and returns only an acknowledgement. Absence
// must be independently observed; neither labels nor that acknowledgement pass.
type PilotCleanupRuntime interface {
	Observe(context.Context, PilotPlan, PilotResource) (PilotResourceObservation, error)
	Delete(context.Context, PilotPlan, PilotResource) error
}

type PilotReconciler struct {
	Journal PilotJournal
	Runtime PilotCleanupRuntime
	Timeout time.Duration
}

func (reconciler *PilotReconciler) validate() error {
	if reconciler == nil || reconciler.Journal == nil || reconciler.Runtime == nil {
		return errors.New("vmrunner: pilot cleanup runtime is unavailable")
	}
	if reconciler.Timeout < time.Millisecond || reconciler.Timeout > 5*time.Minute {
		return errors.New("vmrunner: pilot cleanup budget must be within [1ms, 5m]")
	}
	return nil
}

// Cleanup detaches run cancellation but keeps one finite budget for all owned
// resources. A retry reconciles only existing obligations and creates nothing.
func (reconciler *PilotReconciler) Cleanup(ctx context.Context, runID string) error {
	if err := reconciler.validate(); err != nil {
		return err
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), reconciler.Timeout)
	defer cancel()
	value, err := reconciler.Journal.PilotExecution(cleanup, runID)
	if err != nil {
		return err
	}
	if value.Cleaned {
		return nil
	}
	if ctx.Err() != nil {
		if err := reconciler.Journal.FailPilot(cleanup, runID, "canceled"); err != nil {
			return err
		}
	}
	if err := reconciler.Journal.BeginPilotCleanup(cleanup, runID); err != nil {
		return err
	}
	resources, err := reconciler.Journal.PilotResources(cleanup, runID)
	if err != nil {
		return err
	}
	if len(resources) > MaxPilotResources {
		return ErrExecutionOutstanding
	}
	var failures []error
	for _, resource := range resources {
		if resource.Intent.Parent != "" || resource.Cleaned {
			continue
		}
		if err := reconciler.cleanupResource(cleanup, value.Plan, resource); err != nil {
			failures = append(failures, err)
		}
	}
	// A late child may have survived a parent in an earlier attempt. Re-read
	// the durable list rather than relying on the snapshot taken before deletes.
	resources, err = reconciler.Journal.PilotResources(cleanup, runID)
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	for _, resource := range resources {
		if resource.Cleaned {
			continue
		}
		if err := reconciler.cleanupResource(cleanup, value.Plan, resource); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) != 0 {
		return errors.Join(failures...)
	}
	return reconciler.Journal.CompletePilotCleanup(cleanup, runID)
}

func (reconciler *PilotReconciler) cleanupResource(ctx context.Context, plan PilotPlan, resource PilotResource) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	runID := plan.Spec.RunID
	if err := ValidatePilotResource(plan, resource.Intent); err != nil {
		return err
	}
	if err := reconciler.Journal.BeginPilotResourceCleanup(ctx, runID, resource.Intent.Key); err != nil {
		return err
	}
	if !resource.Submitted || resource.Rejected {
		return reconciler.Journal.CompletePilotResourceCleanup(ctx, runID, resource.Intent.Key, resource.Receipt)
	}
	observation, err := reconciler.Runtime.Observe(ctx, plan, resource)
	if err != nil {
		return errors.Join(ErrExecutionOutstanding, err)
	}
	if resource.Receipt.UID == "" {
		// NotFound cannot settle an ambiguous create. A late exact receipt can
		// bind during cleanup, after which UID-qualified deletion is possible.
		if observation.Absent || observation.Receipt.UID == "" {
			return ErrExecutionOutstanding
		}
		if err := reconciler.Journal.BindPilotResource(ctx, runID, resource.Intent.Key, observation.Receipt); err != nil {
			return err
		}
		resource.Receipt = observation.Receipt
	}
	if err := checkPilotObservation(resource, observation); err != nil {
		return err
	}
	if err := reconciler.recordPods(ctx, plan, resource, observation.Pods); err != nil {
		return err
	}
	if !observation.Absent {
		if err := reconciler.Runtime.Delete(ctx, plan, resource); err != nil {
			return errors.Join(ErrExecutionOutstanding, err)
		}
	}
	children, err := reconciler.Journal.PilotResources(ctx, runID)
	if err != nil {
		return err
	}
	for _, child := range children {
		if child.Intent.Parent != resource.Intent.Key || child.Cleaned {
			continue
		}
		if err := reconciler.cleanupResource(ctx, plan, child); err != nil {
			return err
		}
	}
	observation, err = reconciler.Runtime.Observe(ctx, plan, resource)
	if err != nil {
		return errors.Join(ErrExecutionOutstanding, err)
	}
	if err := checkPilotObservation(resource, observation); err != nil {
		return err
	}
	if err := reconciler.recordPods(ctx, plan, resource, observation.Pods); err != nil {
		return err
	}
	if !observation.Absent || len(observation.Pods) != 0 {
		return ErrExecutionOutstanding
	}
	return reconciler.Journal.CompletePilotResourceCleanup(ctx, runID, resource.Intent.Key, resource.Receipt)
}

func checkPilotObservation(resource PilotResource, observation PilotResourceObservation) error {
	if len(observation.Pods) > MaxPilotResources {
		return ErrExecutionOutstanding
	}
	if observation.Absent {
		if observation.Receipt.UID != "" {
			return errors.New("vmrunner: absent resource carries an incompatible binding")
		}
		return nil
	}
	actual := observation.Receipt
	owned := resource.Receipt
	if actual.UID != owned.UID || actual.SpecIdentity != owned.SpecIdentity || actual.OwnerUID != owned.OwnerUID || actual.PodIP != owned.PodIP || !actual.CreatedAt.Equal(owned.CreatedAt) {
		return errors.New("vmrunner: observed pilot resource differs from its owned UID/spec")
	}
	return nil
}

func (reconciler *PilotReconciler) recordPods(ctx context.Context, plan PilotPlan, parent PilotResource, pods []PilotOwnedPod) error {
	if len(pods) > MaxPilotResources {
		return ErrExecutionOutstanding
	}
	if len(pods) != 0 && parent.Intent.Kind != "Job" && parent.Intent.Kind != "VirtualMachineInstance" {
		return errors.New("vmrunner: unexpected pilot resource children")
	}
	var failures []error
	for _, pod := range pods {
		if pod.Intent.Parent != parent.Intent.Key || pod.Receipt.OwnerUID != parent.Receipt.UID {
			return errors.New("vmrunner: observed child has foreign ownership")
		}
		if err := reconciler.Journal.ObservePilotPod(ctx, plan.Spec.RunID, pod.Intent, pod.Receipt); err != nil {
			failures = append(failures, fmt.Errorf("vmrunner: retain pilot child: %w", err))
		}
	}
	return errors.Join(failures...)
}

func (reconciler *PilotReconciler) Recover(ctx context.Context) error {
	if err := reconciler.validate(); err != nil {
		return err
	}
	pending, err := reconciler.Journal.PendingPilots(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, value := range pending {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if err := reconciler.Cleanup(ctx, value.Plan.Spec.RunID); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
