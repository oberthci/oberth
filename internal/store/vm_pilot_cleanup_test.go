package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/vmrunner"
)

type pilotCleanupRuntime struct {
	objects         map[string]vmrunner.PilotResource
	deleted         []string
	keepAfterDelete bool
	deleteErr       error
	waitForDeadline bool
}

func (runtime *pilotCleanupRuntime) Observe(ctx context.Context, _ vmrunner.PilotPlan, resource vmrunner.PilotResource) (vmrunner.PilotResourceObservation, error) {
	if runtime.waitForDeadline {
		<-ctx.Done()
		return vmrunner.PilotResourceObservation{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return vmrunner.PilotResourceObservation{}, err
	}
	actual, exists := runtime.objects[resource.Intent.Key]
	observation := vmrunner.PilotResourceObservation{Absent: !exists}
	if exists {
		observation.Receipt = actual.Receipt
	}
	owner := resource.Receipt.UID
	if owner == "" {
		owner = actual.Receipt.UID
	}
	for _, child := range runtime.objects {
		if child.Intent.Parent == resource.Intent.Key && child.Receipt.OwnerUID == owner {
			observation.Pods = append(observation.Pods, vmrunner.PilotOwnedPod{Intent: child.Intent, Receipt: child.Receipt})
		}
	}
	return observation, nil
}

func (runtime *pilotCleanupRuntime) Delete(ctx context.Context, _ vmrunner.PilotPlan, resource vmrunner.PilotResource) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	actual, exists := runtime.objects[resource.Intent.Key]
	if !exists || actual.Receipt.UID != resource.Receipt.UID {
		return errors.New("UID precondition failed")
	}
	runtime.deleted = append(runtime.deleted, resource.Intent.Key)
	if runtime.deleteErr != nil {
		return runtime.deleteErr
	}
	if !runtime.keepAfterDelete {
		delete(runtime.objects, resource.Intent.Key)
	}
	return nil
}

func fixturePilotRuntime(t *testing.T, f *pilotFixture) *pilotCleanupRuntime {
	t.Helper()
	resources, err := f.s.PilotResources(context.Background(), f.plan.Spec.RunID)
	mustPilot(t, err)
	runtime := &pilotCleanupRuntime{objects: make(map[string]vmrunner.PilotResource)}
	for _, resource := range resources {
		if resource.Receipt.UID != "" && !resource.Cleaned {
			runtime.objects[resource.Intent.Key] = resource
		}
	}
	return runtime
}

func TestPilotCleanupCanceledContextStillRemovesExactOwnedResources(t *testing.T) {
	f := newPilotFixture(t)
	f.start(t)
	f.create(t, vmrunner.InitialVMResource, "VirtualMachineInstance")
	f.pod(t, "pod-beacon-0", vmrunner.InitialVMResource)
	f.conductor(t)
	runtime := fixturePilotRuntime(t, f)
	reconciler := vmrunner.PilotReconciler{Journal: f.s, Runtime: runtime, Timeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mustPilot(t, reconciler.Cleanup(ctx, f.plan.Spec.RunID))
	value, err := f.s.PilotExecution(context.Background(), f.plan.Spec.RunID)
	mustPilot(t, err)
	if !value.Cleaned || value.Failure != "canceled" || len(runtime.objects) != 0 || len(runtime.deleted) != 4 {
		t.Fatalf("canceled cleanup lost ownership: %#v, deleted=%v, remain=%v", value, runtime.deleted, runtime.objects)
	}
	if _, err := f.s.CompletedPilotReceipt(context.Background(), f.plan.Spec.RunID); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("canceled cleanup manufactured success: %v", err)
	}
}

func TestPilotCleanupAcknowledgementFailureAndReplacementCannotReleaseCapacity(t *testing.T) {
	for _, mode := range []string{"ack-only", "delete-error", "replacement", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			f := newPilotFixture(t)
			f.start(t)
			f.create(t, vmrunner.InitialVMResource, "VirtualMachineInstance")
			runtime := fixturePilotRuntime(t, f)
			switch mode {
			case "ack-only":
				runtime.keepAfterDelete = true
			case "delete-error":
				runtime.deleteErr = errors.New("delete unavailable")
			case "replacement":
				actual := runtime.objects[vmrunner.InitialVMResource]
				actual.Receipt.UID = "foreign-replacement"
				runtime.objects[vmrunner.InitialVMResource] = actual
			case "timeout":
				runtime.waitForDeadline = true
			}
			reconciler := vmrunner.PilotReconciler{Journal: f.s, Runtime: runtime, Timeout: 10 * time.Millisecond}
			if err := reconciler.Cleanup(context.Background(), f.plan.Spec.RunID); err == nil {
				t.Fatalf("%s completed cleanup", mode)
			}
			value, err := f.s.PilotExecution(context.Background(), f.plan.Spec.RunID)
			mustPilot(t, err)
			if value.Cleaned {
				t.Fatal("failed cleanup released execution")
			}
			if mode == "replacement" && len(runtime.deleted) != 0 {
				t.Fatalf("foreign object deleted: %v", runtime.deleted)
			}
			legacy := vmIntent(t, f.s, f.repo, "next")
			if _, err := f.s.ReserveVMExecution(context.Background(), legacy); !errors.Is(err, ErrVMCapacity) {
				t.Fatalf("capacity released: %v", err)
			}
		})
	}
}

func TestPilotCleanupReconcilesLateUnknownCreateAndChild(t *testing.T) {
	ctx := context.Background()
	f := newPilotFixture(t)
	f.start(t)
	intent := f.intent(vmrunner.InitialVMResource, "VirtualMachineInstance", "")
	mustPilot(t, f.s.AddPilotResource(ctx, f.plan.Spec.RunID, intent))
	mustPilot(t, f.s.SubmitPilotResource(ctx, f.plan.Spec.RunID, intent.Key))
	runtime := fixturePilotRuntime(t, f)
	reconciler := vmrunner.PilotReconciler{Journal: f.s, Runtime: runtime, Timeout: time.Second}
	if err := reconciler.Recover(ctx); !errors.Is(err, vmrunner.ErrExecutionOutstanding) {
		t.Fatalf("absence settled unknown create: %v", err)
	}
	runtime.objects[intent.Key] = vmrunner.PilotResource{Intent: intent, Receipt: vmrunner.ResourceReceipt{UID: "late-vmi", SpecIdentity: intent.SpecIdentity, CreatedAt: *f.now}, Submitted: true}
	pod := f.intent("late-launcher", "Pod", intent.Key)
	runtime.objects[pod.Key] = vmrunner.PilotResource{Intent: pod, Receipt: vmrunner.ResourceReceipt{UID: "late-pod", OwnerUID: "late-vmi", SpecIdentity: pod.SpecIdentity, CreatedAt: *f.now}, Submitted: true}
	mustPilot(t, reconciler.Recover(ctx))
	resources, err := f.s.PilotResources(ctx, f.plan.Spec.RunID)
	mustPilot(t, err)
	if len(resources) != 2 || len(runtime.deleted) != 2 || len(runtime.objects) != 0 {
		t.Fatalf("late obligations dropped: %#v, deleted=%v", resources, runtime.deleted)
	}
	for _, resource := range resources {
		if !resource.Cleaned || resource.Receipt.UID == "" {
			t.Fatalf("unbound cleanup: %#v", resource)
		}
	}
}
