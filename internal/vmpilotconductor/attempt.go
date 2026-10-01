package vmpilotconductor

import (
	"context"
	"errors"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/vmrunner"
)

// ErrProcessPending means no authoritative process/termination exists yet.
// Pod phase and Job conditions never substitute for container status.
var ErrProcessPending = errors.New("conductor: process observation pending")

func (backend *Backend) active(ctx context.Context, plan vmrunner.PilotPlan) (vmrunner.PilotExecution, error) {
	value, err := backend.execution(ctx, plan)
	if err != nil {
		return value, err
	}
	if !value.Submitted || value.Cleaning || value.Cleaned || value.Failure != "" {
		return value, errors.New("conductor: execution is inactive")
	}
	if !time.Now().Before(value.CreatedAt.Add(plan.Spec.Deadline)) {
		return value, errors.New("conductor: execution deadline elapsed")
	}
	return value, nil
}

// process reads the actual Job and sole owner-UID Pod on every call. All
// observed children are retained before an unexpected second child is refused.
func (backend *Backend) process(ctx context.Context, plan vmrunner.PilotPlan, execution vmrunner.PilotExecution) (vmrunner.ConductorAttempt, corev1.ContainerStatus, error) {
	var attempt vmrunner.ConductorAttempt
	var status corev1.ContainerStatus
	job, err := backend.resource(ctx, plan, vmrunner.ConductorResource)
	if err != nil {
		return attempt, status, err
	}
	if job.Receipt.UID == "" || job.Cleaning || job.Cleaned || job.Rejected {
		return attempt, status, mismatch("Job binding")
	}
	call, cancel := context.WithTimeout(ctx, backend.config.RequestTimeout)
	defer cancel()
	object, err := backend.client.Resource(jobs).Namespace(plan.ConductorNamespace).Get(call, job.Intent.Name, metav1.GetOptions{})
	if err != nil {
		return attempt, status, err
	}
	receipt, err := backend.jobReceipt(plan, object)
	if err != nil {
		return attempt, status, err
	}
	if !sameReceipt(receipt, job.Receipt) || object.GetDeletionTimestamp() != nil {
		return attempt, status, mismatch("Job UID or deletion")
	}
	children, err := backend.children(ctx, plan, job)
	if err != nil {
		return attempt, status, err
	}
	var failures []error
	var owned vmrunner.PilotOwnedPod
	for _, object := range children {
		pod, err := backend.podReceipt(plan, job, object)
		if err == nil {
			err = backend.journal.ObservePilotPod(ctx, plan.Spec.RunID, pod.Intent, pod.Receipt)
		}
		if err != nil {
			failures = append(failures, err)
		}
		owned = pod
	}
	if err := errors.Join(failures...); err != nil {
		return attempt, status, err
	}
	if len(children) == 0 && execution.Attempt == nil {
		return attempt, status, ErrProcessPending
	}
	if len(children) != 1 {
		return attempt, status, mismatch("sole Pod")
	}
	pod, err := backend.validatePod(plan, children[0], job.Receipt.UID)
	if err != nil {
		return attempt, status, err
	}
	if len(pod.Status.InitContainerStatuses) != 0 || len(pod.Status.EphemeralContainerStatuses) != 0 || len(pod.Status.ContainerStatuses) > 1 {
		return attempt, status, mismatch("container status inventory")
	}
	if len(pod.Status.ContainerStatuses) == 0 {
		if execution.Attempt != nil {
			return attempt, status, mismatch("missing bound process")
		}
		return attempt, status, ErrProcessPending
	}
	status = pod.Status.ContainerStatuses[0]
	if status.Name != "conductor" || status.RestartCount != 0 || status.LastTerminationState != (corev1.ContainerState{}) {
		return attempt, status, mismatch("sole container attempt")
	}
	if status.State.Waiting != nil {
		if execution.Attempt != nil || status.State.Running != nil || status.State.Terminated != nil {
			return attempt, status, mismatch("replaced container state")
		}
		return attempt, status, ErrProcessPending
	}
	_, digest, _ := strings.Cut(plan.ConductorImageRef, "@")
	if status.Image != plan.ConductorImageRef || (status.ImageID != digest && status.ImageID != plan.ConductorImageRef && status.ImageID != "docker-pullable://"+plan.ConductorImageRef) {
		return attempt, status, mismatch("actual container image")
	}
	var started time.Time
	switch {
	case status.State.Running != nil && status.State.Terminated == nil:
		started = status.State.Running.StartedAt.Time
	case status.State.Terminated != nil && status.State.Running == nil:
		started = status.State.Terminated.StartedAt.Time
		if status.State.Terminated.ContainerID != status.ContainerID {
			return attempt, status, mismatch("terminated container identity")
		}
	default:
		return attempt, status, mismatch("container state")
	}
	if started.Before(owned.Receipt.CreatedAt) || observationPredates(started, execution.CreatedAt) || !started.Before(execution.CreatedAt.Add(plan.Spec.Deadline)) {
		return attempt, status, mismatch("process start")
	}
	attempt = vmrunner.ConductorAttempt{JobUID: job.Receipt.UID, PodUID: owned.Receipt.UID, PodName: pod.Name, NodeName: pod.Spec.NodeName, Container: "conductor", ContainerID: status.ContainerID, ImageDigest: digest, SpecIdentity: owned.Intent.SpecIdentity, StartedAt: started}
	attempt.AttemptID = identity(attempt)
	if err := vmrunner.ValidateConductorAttempt(plan, attempt); err != nil {
		return attempt, status, err
	}
	return attempt, status, nil
}

func (backend *Backend) fail(ctx context.Context, plan vmrunner.PilotPlan, err error) error {
	if errors.Is(err, ErrProcessPending) {
		return err
	}
	return errors.Join(err, backend.journal.FailPilot(ctx, plan.Spec.RunID, "runtime"))
}

// BindAttempt persists the sole actual process. This is not an attach handle or
// success receipt, and it conveys no authority to run a guest-selected command.
func (backend *Backend) BindAttempt(ctx context.Context, plan vmrunner.PilotPlan) (vmrunner.ConductorAttempt, error) {
	execution, err := backend.active(ctx, plan)
	if err != nil {
		return vmrunner.ConductorAttempt{}, err
	}
	attempt, _, err := backend.process(ctx, plan, execution)
	if err != nil {
		return vmrunner.ConductorAttempt{}, backend.fail(ctx, plan, err)
	}
	if err := backend.journal.BindConductorAttempt(ctx, plan.Spec.RunID, attempt); err != nil {
		return vmrunner.ConductorAttempt{}, err
	}
	return attempt, nil
}

// Termination reads the exact bound process's real exit. Failed exits retain
// their actual value for diagnosis but return an error and poison the journal.
// Even an accepted exit is provisional: this package produces no PilotReceipt.
func (backend *Backend) Termination(ctx context.Context, plan vmrunner.PilotPlan, bound vmrunner.ConductorAttempt) (vmrunner.ConductorTermination, error) {
	var result vmrunner.ConductorTermination
	execution, err := backend.active(ctx, plan)
	if err != nil {
		return result, err
	}
	if execution.Attempt == nil || vmrunner.ConductorAttemptIdentity(*execution.Attempt) != vmrunner.ConductorAttemptIdentity(bound) {
		return result, mismatch("durable attempt")
	}
	actual, status, err := backend.process(ctx, plan, execution)
	if err != nil {
		return result, backend.fail(ctx, plan, err)
	}
	if vmrunner.ConductorAttemptIdentity(actual) != vmrunner.ConductorAttemptIdentity(bound) {
		return result, backend.fail(ctx, plan, mismatch("actual process identity"))
	}
	terminal := status.State.Terminated
	if terminal == nil {
		return result, ErrProcessPending
	}
	result = vmrunner.ConductorTermination{Attempt: actual, ExitCode: terminal.ExitCode, Signal: terminal.Signal, RestartCount: status.RestartCount, Reason: terminal.Reason, FinishedAt: terminal.FinishedAt.Time}
	if result.ExitCode != 0 || result.Signal != 0 || result.Reason != "Completed" || result.FinishedAt.Before(actual.StartedAt) || result.FinishedAt.After(time.Now()) || result.FinishedAt.After(execution.CreatedAt.Add(plan.Spec.Deadline)) {
		return result, backend.fail(ctx, plan, mismatch("successful process termination"))
	}
	return result, nil
}
