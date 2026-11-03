package vmrunner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Controller manages VM lifecycle on behalf of pipeline steps. It is
// operator-owned: the pipeline declares what to test, the controller
// decides how to run it. The controller creates VMIs through its own
// Kubernetes identity, not the pipeline's.
//
// KubeVirtBackend provides the fixed offline lifecycle transport.
// ManagedController couples it to durable create and cleanup obligations.
// Production suite authority and scheduler wiring are separate concerns.
type Controller struct {
	config    Config
	backend   VMBackend
	namespace string
}

// Config is the administrator-owned half of the VM runner. Every field that
// decides trust is here, not in the repository document.
type Config struct {
	// Namespace is where VMIs are created. Must be the pipeline namespace
	// (same as build Jobs), never the server namespace.
	Namespace string

	// MaxCPUCores caps the per-VM CPU allocation.
	// Zero selects the default MaxVMCPUCores.
	MaxCPUCores int

	// MaxMemoryMiB caps the per-VM memory allocation.
	// Zero selects the default MaxVMMemoryMiB.
	MaxMemoryMiB int

	// MaxDiskGiB caps the per-VM disk allocation.
	// Zero selects the default MaxVMDiskGiB.
	MaxDiskGiB int

	// MaxDeadline caps the per-VM execution deadline.
	// Zero selects the default MaxDeadline.
	MaxDeadline time.Duration

	// OrphanGrace is the grace added to MaxDeadline before age-based sweeping.
	// Durable run cancellation should reconcile owned resources earlier.
	// Zero selects one hour.
	OrphanGrace time.Duration

	// CleanupTimeout bounds cleanup even when the execution context is canceled.
	// A timeout leaves an outstanding durable cleanup obligation, never success.
	CleanupTimeout time.Duration
}

func (config *Config) applyDefaults() {
	if config.MaxCPUCores <= 0 {
		config.MaxCPUCores = MaxVMCPUCores
	}
	if config.MaxMemoryMiB <= 0 {
		config.MaxMemoryMiB = MaxVMMemoryMiB
	}
	if config.MaxDiskGiB <= 0 {
		config.MaxDiskGiB = MaxVMDiskGiB
	}
	if config.MaxDeadline <= 0 {
		config.MaxDeadline = MaxDeadline
	}
	if config.OrphanGrace <= 0 {
		config.OrphanGrace = time.Hour
	}
	if config.CleanupTimeout == 0 {
		config.CleanupTimeout = 30 * time.Second
	}
}

// Validate rejects a configuration that could not produce a safe submission.
func (config Config) Validate() error {
	var problems []error
	if strings.TrimSpace(config.Namespace) == "" {
		problems = append(problems, errors.New("vmrunner: Namespace is required"))
	}
	if config.MaxCPUCores < 1 || config.MaxCPUCores > 64 {
		problems = append(problems, fmt.Errorf("vmrunner: MaxCPUCores %d is outside [1, 64]", config.MaxCPUCores))
	}
	if config.MaxMemoryMiB < MinVMMemoryMiB || config.MaxMemoryMiB > 131072 {
		problems = append(problems, fmt.Errorf("vmrunner: MaxMemoryMiB %d is outside [%d, 131072]", config.MaxMemoryMiB, MinVMMemoryMiB))
	}
	if config.MaxDiskGiB < 0 || config.MaxDiskGiB > 1024 {
		problems = append(problems, fmt.Errorf("vmrunner: MaxDiskGiB %d is outside [0, 1024]", config.MaxDiskGiB))
	}
	if config.MaxDeadline < MinDeadline {
		problems = append(problems, fmt.Errorf("vmrunner: MaxDeadline %s is below the %s minimum", config.MaxDeadline, MinDeadline))
	}
	if config.CleanupTimeout < time.Millisecond || config.CleanupTimeout > 5*time.Minute {
		problems = append(problems, errors.New("vmrunner: CleanupTimeout must be within [1ms, 5m]"))
	}
	return errors.Join(problems...)
}

// VMBackend is the narrow interface through which the controller interacts
// with KubeVirt. It is separated from the Kubernetes client so tests can
// supply a fake without needing KubeVirt CRDs.
//
// KubeVirtBackend implements this interface for an operator-owned offline guest.
type VMBackend interface {
	// CreateVMI creates a VirtualMachineInstance from the given spec. It
	// returns the VMI's exact ownership receipt on success. The backend is responsible
	// for constructing the VMI with the security baseline documented in
	// docs/solutions/kubevirt-runner.md.
	CreateVMI(ctx context.Context, namespace string, spec VMRunSpec) (VMInstance, error)

	// GetVMI returns the current identity and phase of a VMI. Terminal phases are
	// "Succeeded" and "Failed". Non-terminal phases include "Pending",
	// "Scheduling", "Scheduled", and "Running".
	GetVMI(ctx context.Context, namespace, name string) (VMObservation, error)

	// DeleteVMI uses UID preconditions and waits for observed absence of the VMI
	// and its owned launchers. A deletion request alone is not success. A same-name
	// replacement is an error and must never be deleted. Repeated cleanup of an
	// absent owned VMI succeeds only after launcher absence is also established.
	DeleteVMI(ctx context.Context, namespace string, instance VMInstance) error

	// ListVMIs lists verified receipts for VMIs created by this controller (identified by
	// the oberth.ci/tier=vm-runner label) that are older than the given age.
	ListVMIs(ctx context.Context, namespace string, olderThan time.Duration) ([]VMInstance, error)
}

// NewController builds the VM runner controller. The backend may be nil in
// tests that only exercise admission and lifecycle state transitions.
func NewController(config Config, backend VMBackend) (*Controller, error) {
	config.applyDefaults()
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Controller{
		config:    config,
		backend:   backend,
		namespace: config.Namespace,
	}, nil
}

// Namespace reports where this controller creates VMIs.
func (controller *Controller) Namespace() string { return controller.namespace }

// Admit validates a VM run spec against the controller's configuration.
// This runs before any VMI exists.
func (controller *Controller) Admit(spec VMRunSpec) error {
	if err := ValidateVMRunSpec(spec); err != nil {
		return err
	}
	// Enforce controller-specific ceilings that may be tighter than the
	// package-level constants.
	var problems []error
	if spec.Resources.CPUCores > controller.config.MaxCPUCores {
		problems = append(problems, fmt.Errorf(
			"vmrunner: CPUCores %d exceeds the configured %d ceiling",
			spec.Resources.CPUCores, controller.config.MaxCPUCores))
	}
	if spec.Resources.MemoryMiB > controller.config.MaxMemoryMiB {
		problems = append(problems, fmt.Errorf(
			"vmrunner: MemoryMiB %d exceeds the configured %d ceiling",
			spec.Resources.MemoryMiB, controller.config.MaxMemoryMiB))
	}
	if spec.Resources.DiskGiB > controller.config.MaxDiskGiB {
		problems = append(problems, fmt.Errorf(
			"vmrunner: DiskGiB %d exceeds the configured %d ceiling",
			spec.Resources.DiskGiB, controller.config.MaxDiskGiB))
	}
	if spec.Deadline > controller.config.MaxDeadline {
		problems = append(problems, fmt.Errorf(
			"vmrunner: Deadline %s exceeds the configured %s ceiling",
			spec.Deadline, controller.config.MaxDeadline))
	}
	return errors.Join(problems...)
}

// Create validates and submits a VM execution. It returns a UID-bound receipt on
// success. The VMI is created with the security baseline documented in
// docs/solutions/kubevirt-runner.md.
func (controller *Controller) Create(ctx context.Context, spec VMRunSpec) (VMInstance, error) {
	if controller.backend == nil {
		return VMInstance{}, errors.New("vmrunner: no backend configured")
	}
	if err := controller.Admit(spec); err != nil {
		return VMInstance{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, spec.Deadline)
	defer cancel()
	instance, err := controller.backend.CreateVMI(ctx, controller.namespace, spec)
	if err != nil {
		return instance, fmt.Errorf("vmrunner: create VMI: %w", err)
	}
	if err := validateInstance(instance, spec); err != nil {
		// Do not invent ownership for an incomplete create response. The caller's
		// persisted create intent must reconcile this ambiguous obligation.
		return instance, err
	}
	return instance, nil
}

// Wait supervises a VMI to its terminal phase and returns the result.
// It polls the VMI phase until it reaches "Succeeded" or "Failed", or
// until the context is cancelled.
func (controller *Controller) Wait(ctx context.Context, instance VMInstance, spec VMRunSpec) (result VMRunResult, err error) {
	result = VMRunResult{Phase: "Failed", ExitCode: ExitCodeUnknown,
		Evidence: VMEvidence{ExitCodeSource: ExitCodeSourceUnknown}}
	if controller.backend == nil {
		return result, errors.New("vmrunner: no backend configured")
	}
	if err := controller.Admit(spec); err != nil {
		return result, err
	}
	if err := validateInstance(instance, spec); err != nil {
		return result, err
	}
	result = VMRunResult{
		Phase: "Failed", ExitCode: ExitCodeUnknown,
		Evidence: VMEvidence{
			CandidateSHA: spec.CandidateSHA, SuiteRevision: spec.SuiteRevision,
			GuestImageRef: spec.GuestImageRef, VMInstanceName: instance.Name,
			VMInstanceUID: instance.UID, StartedAt: instance.CreatedAt,
			ExitCodeSource: ExitCodeSourceUnknown,
		},
	}
	defer func() {
		result.Evidence.FinishedAt = time.Now().UTC()
		result.Duration = result.Evidence.FinishedAt.Sub(instance.CreatedAt)
		cleanupErr := controller.Cancel(ctx, instance)
		result.CleanupComplete = cleanupErr == nil
		if cleanupErr != nil {
			result.Phase = "Failed"
			err = errors.Join(err, fmt.Errorf("vmrunner: cleanup VMI %s: %w", instance.Name, cleanupErr))
		}
	}()
	// The deadline starts at creation, including across a resumed wait.
	ctx, cancel := context.WithDeadline(ctx, instance.CreatedAt.Add(spec.Deadline))
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("vmrunner: wait VMI %s: %w", instance.Name, err)
		}
		observation, err := controller.backend.GetVMI(ctx, controller.namespace, instance.Name)
		if err != nil {
			return result, fmt.Errorf("vmrunner: read VMI %s: %w", instance.Name, err)
		}
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("vmrunner: wait VMI %s: %w", instance.Name, err)
		}
		if observation.Instance.Name != instance.Name || observation.Instance.UID != instance.UID ||
			observation.Instance.SpecIdentity != instance.SpecIdentity || !observation.Instance.CreatedAt.Equal(instance.CreatedAt) {
			return result, errors.New("vmrunner: observed VMI identity differs from admitted instance")
		}
		if isTerminal(observation.Phase) {
			result.Phase = observation.Phase
			return result, nil
		}
		select {
		case <-ctx.Done():
			return result, fmt.Errorf("vmrunner: wait VMI %s: %w", instance.Name, ctx.Err())
		case <-ticker.C:
		}
	}
}

// Cancel performs bounded UID-bound cleanup independent of run cancellation.
func (controller *Controller) Cancel(ctx context.Context, instance VMInstance) error {
	if controller.backend == nil {
		return errors.New("vmrunner: no backend configured for cleanup")
	}
	if instance.Name == "" || instance.UID == "" || instance.SpecIdentity == "" {
		return errors.New("vmrunner: cleanup requires exact VMI ownership")
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), controller.config.CleanupTimeout)
	defer cancel()
	err := controller.backend.DeleteVMI(cleanup, controller.namespace, instance)
	return errors.Join(err, cleanup.Err())
}

// SweepOrphanedVMIs deletes VMIs that are older than the configured grace
// window. This catches VMIs that were orphaned by a server crash or restart.
func (controller *Controller) SweepOrphanedVMIs(ctx context.Context) (int, error) {
	if controller.backend == nil {
		return 0, nil
	}
	listCtx, cancel := context.WithTimeout(ctx, controller.config.CleanupTimeout)
	defer cancel()
	// An age-only sweep must not kill a valid long-running admitted execution.
	names, err := controller.backend.ListVMIs(listCtx, controller.namespace, controller.config.MaxDeadline+controller.config.OrphanGrace)
	if err != nil {
		return 0, fmt.Errorf("vmrunner: list orphaned VMIs: %w", err)
	}
	var swept int
	for _, instance := range names {
		if ctx.Err() != nil {
			return swept, errors.Join(err, ctx.Err())
		}
		if deleteErr := controller.Cancel(ctx, instance); deleteErr != nil {
			err = errors.Join(err, deleteErr)
		} else {
			swept++
		}
	}
	return swept, err
}

func validateInstance(instance VMInstance, spec VMRunSpec) error {
	if instance.Name == "" || instance.UID == "" || instance.CreatedAt.IsZero() ||
		instance.CreatedAt.After(time.Now()) || instance.SpecIdentity != SpecIdentity(spec) {
		return errors.New("vmrunner: VMI ownership does not match admitted spec")
	}
	return nil
}

// isTerminal reports whether a VMI phase is terminal.
func isTerminal(phase string) bool {
	return phase == "Succeeded" || phase == "Failed"
}
