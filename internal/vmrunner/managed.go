package vmrunner

import (
	"context"
	"errors"
	"fmt"
)

var (
	ErrExecutionOutstanding = errors.New("vmrunner: durable execution or cleanup remains outstanding")
	ErrCreateRefused        = errors.New("vmrunner: create was definitively refused")
)

// ManagedController couples the lifecycle controller to its durable journal.
// It does not report suite success: callers still require independently
// authorized suite inputs and a trusted process receipt before publication.
type ManagedController struct {
	Controller *Controller
	Journal    ExecutionJournal
}

// InstanceName is fixed before the API call so a lost create response can be
// reconciled without inventing a resource name or adopting a different spec.
func InstanceName(spec VMRunSpec) string { return "oberth-vm-" + SpecIdentity(spec)[:40] }

func (managed *ManagedController) Execute(ctx context.Context, spec VMRunSpec, profileIdentity string) (result VMRunResult, err error) {
	result.ExitCode = ExitCodeUnknown
	if managed == nil || managed.Controller == nil || managed.Controller.backend == nil || managed.Journal == nil {
		return result, errors.New("vmrunner: lifecycle journal is unavailable")
	}
	if err := managed.Controller.Admit(spec); err != nil {
		return result, err
	}
	value, err := managed.Journal.ReserveVMExecution(ctx, ExecutionIntent{Spec: spec, Namespace: managed.Controller.Namespace(), Name: InstanceName(spec), ProfileIdentity: profileIdentity})
	if err != nil {
		return result, err
	}
	if value.Submitted || value.Cleaning || value.Cleaned {
		return result, ErrExecutionOutstanding
	}
	// The successful CAS is the sole right to submit this create. A concurrent
	// caller that loses it must not cancel the winner's active execution.
	if err := managed.Journal.SubmitVMExecution(ctx, spec.RunID); err != nil {
		return result, err
	}
	defer func() {
		cleanupErr := managed.Cleanup(ctx, spec.RunID)
		result.CleanupComplete = cleanupErr == nil
		if cleanupErr != nil {
			result.Phase = "Failed"
			err = errors.Join(err, cleanupErr)
		}
	}()
	instance, createErr := managed.Controller.Create(ctx, spec)
	// Bind a returned receipt even if cancellation happened after API create.
	// An incomplete/ambiguous receipt remains reserved for recovery.
	if instance.UID != "" {
		binding, cancel := context.WithTimeout(context.WithoutCancel(ctx), managed.Controller.config.CleanupTimeout)
		bindErr := managed.Journal.BindVMExecution(binding, spec.RunID, instance)
		cancel()
		if bindErr != nil {
			return result, errors.Join(createErr, fmt.Errorf("vmrunner: persist instance: %w", bindErr))
		}
	}
	if createErr != nil {
		if errors.Is(createErr, ErrCreateRefused) && instance.UID == "" {
			refusal, cancel := context.WithTimeout(context.WithoutCancel(ctx), managed.Controller.config.CleanupTimeout)
			refusedErr := managed.Journal.RejectVMExecution(refusal, spec.RunID)
			cancel()
			createErr = errors.Join(createErr, refusedErr)
		}
		return result, createErr
	}
	return managed.Controller.Wait(ctx, instance, spec)
}

// Cleanup is bounded even after run cancellation. Recovery may find an object
// for a submitted intent, but absence never settles an unknown create. Only a
// durable UID plus observed deletion (or an intent never submitted) can finish.
func (managed *ManagedController) Cleanup(ctx context.Context, runID string) error {
	if managed == nil || managed.Controller == nil || managed.Journal == nil {
		return errors.New("vmrunner: cleanup journal is unavailable")
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), managed.Controller.config.CleanupTimeout)
	defer cancel()
	value, err := managed.Journal.VMExecution(cleanup, runID)
	if err != nil {
		return err
	}
	if value.Intent.Namespace != managed.Controller.Namespace() {
		return errors.New("vmrunner: cleanup namespace differs from durable intent")
	}
	if value.Cleaned {
		return nil
	}
	if err := managed.Journal.BeginVMCleanup(cleanup, runID); err != nil {
		return err
	}
	if value.Submitted && !value.Rejected && value.Instance.UID == "" {
		if managed.Controller.backend == nil {
			return ErrExecutionOutstanding
		}
		observation, err := managed.Controller.backend.GetVMI(cleanup, value.Intent.Namespace, value.Intent.Name)
		if err != nil {
			return errors.Join(ErrExecutionOutstanding, err)
		}
		if observation.Instance.Name != value.Intent.Name || validateInstance(observation.Instance, value.Intent.Spec) != nil {
			return errors.New("vmrunner: recovered object differs from durable intent")
		}
		if err := managed.Journal.BindVMExecution(cleanup, runID, observation.Instance); err != nil {
			return err
		}
		value.Instance = observation.Instance
	}
	if value.Instance.UID != "" {
		// Use this budget directly; Controller.Cancel detaches cancellation and
		// is appropriate for the original run context, not a cleanup sub-budget.
		if managed.Controller.backend == nil {
			return ErrExecutionOutstanding
		}
		if err := managed.Controller.backend.DeleteVMI(cleanup, value.Intent.Namespace, value.Instance); err != nil {
			return err
		}
		if err := cleanup.Err(); err != nil {
			return err
		}
	}
	return managed.Journal.CompleteVMCleanup(cleanup, runID, value.Instance)
}

// Recover reconciles persisted obligations after startup. It must run before
// accepting new VM executions; it deliberately produces no passing results.
func (managed *ManagedController) Recover(ctx context.Context) error {
	if managed == nil || managed.Journal == nil {
		return errors.New("vmrunner: recovery journal is unavailable")
	}
	values, err := managed.Journal.PendingVMExecutions(ctx)
	if err != nil {
		return err
	}
	var problems []error
	for _, value := range values {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(problems, err)...)
		}
		if err := managed.Cleanup(ctx, value.Intent.Spec.RunID); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}
