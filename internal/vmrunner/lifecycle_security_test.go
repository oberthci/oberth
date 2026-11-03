package vmrunner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWaitRejectsUnadmittedSpecBeforeObservation(t *testing.T) {
	backend := &fakeBackend{getPhases: []string{"Succeeded"}}
	controller, _ := NewController(validConfig(), backend)
	spec := validSpec()
	spec.CandidateSHA = "wrong"
	result, err := controller.Wait(context.Background(), mustCreate(t, controller, validSpec()), spec)
	if err == nil {
		t.Fatal("Wait accepted an invalid candidate binding")
	}
	if result.ExitCode != ExitCodeUnknown || result.Evidence.ExitCodeSource != ExitCodeSourceUnknown {
		t.Fatalf("invalid wait returned an apparent process exit: %#v", result)
	}
	if backend.getCalls.Load() != 0 {
		t.Fatal("Wait contacted the backend before validating admission")
	}
}

func TestWaitRejectsReplacedUIDAndChangedSpec(t *testing.T) {
	for _, field := range []string{"uid", "candidate", "suite", "guest"} {
		t.Run(field, func(t *testing.T) {
			backend := &fakeBackend{getPhases: []string{"Succeeded"}}
			controller, _ := NewController(validConfig(), backend)
			spec := validSpec()
			instance := mustCreate(t, controller, spec)
			switch field {
			case "uid":
				backend.replaceUID = true
			case "candidate":
				spec.CandidateSHA = strings.Repeat("c", 40)
			case "suite":
				spec.SuiteRevision = strings.Repeat("d", 40)
			case "guest":
				spec.GuestImageRef = "registry.example/replacement@sha256:" + strings.Repeat("e", 64)
			}
			result, err := controller.Wait(context.Background(), instance, spec)
			if err == nil || result.Phase == "Succeeded" {
				t.Fatalf("accepted changed %s identity: %#v, %v", field, result, err)
			}
			if field != "uid" && (backend.getCalls.Load() != 0 || backend.deleteCalls.Load() != 0) {
				t.Fatal("unbound spec reached the backend")
			}
		})
	}
}

func TestWaitCleanupIsBoundedAndRequired(t *testing.T) {
	backend := &fakeBackend{getPhases: []string{"Succeeded"}, blockDelete: true}
	config := validConfig()
	config.CleanupTimeout = 10 * time.Millisecond
	controller, _ := NewController(config, backend)
	started := time.Now()
	result, err := controller.Wait(context.Background(), mustCreate(t, controller, validSpec()), validSpec())
	if !errors.Is(err, context.DeadlineExceeded) || result.CleanupComplete || result.Phase != "Failed" {
		t.Fatalf("cleanup failure allowed success: %#v, %v", result, err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("cleanup exceeded its separate budget")
	}
}

func TestWaitCleansUpTerminalVMI(t *testing.T) {
	backend := &fakeBackend{getPhases: []string{"Succeeded"}}
	controller, _ := NewController(validConfig(), backend)
	result, err := controller.Wait(context.Background(), mustCreate(t, controller, validSpec()), validSpec())
	if err != nil || !result.CleanupComplete || backend.deleteCalls.Load() != 1 {
		t.Fatalf("terminal cleanup = %#v, %v", result, err)
	}
}

func TestCreateRejectsMissingUID(t *testing.T) {
	backend := &fakeBackend{omitUID: true}
	controller, _ := NewController(validConfig(), backend)
	if _, err := controller.Create(context.Background(), validSpec()); err == nil {
		t.Fatal("create accepted an unowned resource")
	}
}

func TestWaitDoesNotRestartDeadline(t *testing.T) {
	backend := &fakeBackend{getPhases: []string{"Succeeded"}}
	controller, _ := NewController(validConfig(), backend)
	spec := validSpec()
	instance := mustCreate(t, controller, spec)
	instance.CreatedAt = time.Now().Add(-spec.Deadline - time.Minute).UTC()
	backend.instance = instance
	result, err := controller.Wait(context.Background(), instance, spec)
	if !errors.Is(err, context.DeadlineExceeded) || result.Phase == "Succeeded" || !result.CleanupComplete {
		t.Fatalf("resumed wait extended the admitted deadline: %#v, %v", result, err)
	}
	if backend.getCalls.Load() != 0 {
		t.Fatal("expired execution reached observation")
	}
}

func TestWaitRetainsCreatedUID(t *testing.T) {
	backend := &fakeBackend{getPhases: []string{"Succeeded"}}
	controller, _ := NewController(validConfig(), backend)
	spec := validSpec()
	name, err := controller.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	result, err := controller.Wait(context.Background(), name, spec)
	if err != nil {
		t.Fatal(err)
	}
	if result.Evidence.VMInstanceUID != "uid-"+spec.RunID {
		t.Fatalf("created UID lost from evidence: %q", result.Evidence.VMInstanceUID)
	}
}

func TestWaitDoesNotInferProcessSuccessFromVMIPhase(t *testing.T) {
	backend := &fakeBackend{getPhases: []string{"Succeeded"}}
	controller, _ := NewController(validConfig(), backend)
	result, err := controller.Wait(context.Background(), mustCreate(t, controller, validSpec()), validSpec())
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == 0 {
		t.Fatal("a successful VM shutdown was reported as a successful suite process")
	}
}

func TestWaitCleansUpAfterObservationFailure(t *testing.T) {
	backend := &fakeBackend{getErr: errors.New("observation unavailable")}
	controller, _ := NewController(validConfig(), backend)
	_, err := controller.Wait(context.Background(), mustCreate(t, controller, validSpec()), validSpec())
	if err == nil || !strings.Contains(err.Error(), "observation unavailable") {
		t.Fatalf("observation error lost: %v", err)
	}
	if backend.deleteCalls.Load() != 1 {
		t.Fatal("observation failure abandoned the VMI")
	}
}

func TestWaitReportsCleanupFailure(t *testing.T) {
	cleanupErr := errors.New("cleanup denied")
	backend := &fakeBackend{getPhases: []string{"Pending"}, deleteErr: cleanupErr}
	controller, _ := NewController(validConfig(), backend)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := controller.Wait(ctx, mustCreate(t, controller, validSpec()), validSpec())
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("cleanup failure discarded: %v", err)
	}
}
