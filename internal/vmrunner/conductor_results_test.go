package vmrunner

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testPilotPlan() PilotPlan {
	spec := validSpec()
	spec.Resources = VMResources{CPUCores: 1, MemoryMiB: 512}
	spec.Deadline = 5 * time.Minute
	return PilotPlan{Version: 1, Profile: BeaconTransportProfile, Spec: spec,
		ConductorImageRef: "registry.example/conductor@sha256:" + strings.Repeat("e", 64),
		KernelDigest:      "sha256:" + strings.Repeat("1", 64), InitramfsDigest: "sha256:" + strings.Repeat("2", 64),
		GuestHelperDigest: "sha256:" + strings.Repeat("3", 64), ArtifactDigest: "sha256:" + strings.Repeat("4", 64), ArtifactBytes: 1024,
		GuestNamespace: "pilot-guests", ConductorNamespace: "pilot-conductor", ServerNamespace: "oberth"}
}

func testConductorAttempt() ConductorAttempt {
	return ConductorAttempt{AttemptID: "attempt-one", JobUID: "job-one", PodUID: "pod-one", Container: "conductor",
		ContainerID: "containerd://container-one", ImageDigest: "sha256:" + strings.Repeat("e", 64),
		SpecIdentity: strings.Repeat("f", 64), StartedAt: time.Now().UTC().Add(-time.Minute)}
}

func testConductorEvents(plan PilotPlan, attempt ConductorAttempt) []ConductorEvent {
	var events []ConductorEvent
	add := func(id, phase string) {
		events = append(events, ConductorEvent{Version: 1, Execution: PilotIdentity(plan), Attempt: ConductorAttemptIdentity(attempt),
			Inventory: PilotInventoryIdentity(plan), Sequence: len(events) + 1, Case: id, Phase: phase})
	}
	for _, id := range BeaconCases() {
		add(id, "start")
		add(id, "pass")
	}
	add("", "complete")
	return events
}

func encodeConductorEvents(t *testing.T, events []ConductorEvent) []byte {
	t.Helper()
	var body bytes.Buffer
	for _, event := range events {
		if err := json.NewEncoder(&body).Encode(event); err != nil {
			t.Fatal(err)
		}
	}
	return body.Bytes()
}

func testPilotReceipt(t *testing.T) PilotReceipt {
	t.Helper()
	plan, attempt := testPilotPlan(), testConductorAttempt()
	results, err := ReadConductorResults(bytes.NewReader(encodeConductorEvents(t, testConductorEvents(plan, attempt))), plan, attempt)
	if err != nil {
		t.Fatal(err)
	}
	return PilotReceipt{Plan: plan, Attempt: attempt, Results: results,
		Termination: ConductorTermination{Attempt: attempt, Reason: "Completed", FinishedAt: attempt.StartedAt.Add(time.Second)}}
}

func TestConductorReceiptRequiresHostExitAndExactSealedInputs(t *testing.T) {
	for _, attack := range []string{"exit-seven", "signal", "oom", "restart", "replacement-pod", "replacement-container", "wrong-image", "source", "artifact", "suite", "guest", "kernel", "helper", "deadline", "missing-case", "duplicate-case", "missing-complete"} {
		t.Run(attack, func(t *testing.T) {
			receipt := testPilotReceipt(t)
			plan := receipt.Plan
			switch attack {
			case "exit-seven":
				receipt.Termination.ExitCode = 7
			case "signal":
				receipt.Termination.Signal = 9
			case "oom":
				receipt.Termination.Reason = "OOMKilled"
			case "restart":
				receipt.Termination.RestartCount = 1
			case "replacement-pod":
				receipt.Termination.Attempt.PodUID = "replacement"
			case "replacement-container":
				receipt.Termination.Attempt.ContainerID = "containerd://replacement"
			case "wrong-image":
				receipt.Attempt.ImageDigest = "sha256:" + strings.Repeat("a", 64)
			case "source":
				plan.Spec.CandidateSHA = strings.Repeat("d", 40)
			case "artifact":
				plan.ArtifactDigest = "sha256:" + strings.Repeat("a", 64)
			case "suite":
				plan.Spec.SuiteRevision = strings.Repeat("d", 40)
			case "guest":
				plan.Spec.GuestImageRef = "registry.example/replaced@sha256:" + strings.Repeat("a", 64)
			case "kernel":
				plan.KernelDigest = "sha256:" + strings.Repeat("a", 64)
			case "helper":
				plan.GuestHelperDigest = "sha256:" + strings.Repeat("a", 64)
			case "deadline":
				receipt.Termination.FinishedAt = receipt.Attempt.StartedAt.Add(plan.Spec.Deadline + time.Nanosecond)
			case "missing-case":
				receipt.Results.Cases = receipt.Results.Cases[1:]
			case "duplicate-case":
				receipt.Results.Cases[1] = receipt.Results.Cases[0]
			case "missing-complete":
				receipt.Results.Complete = false
			}
			if err := VerifyPilotReceipt(plan, receipt); err == nil {
				t.Fatalf("complete-looking success authorized %s", attack)
			}
		})
	}
	receipt := testPilotReceipt(t)
	if err := VerifyPilotReceipt(receipt.Plan, receipt); err != nil {
		t.Fatalf("exact provisional receipt rejected: %v", err)
	}
}

func TestConductorResultStreamRejectsIncompleteAndForgedEvents(t *testing.T) {
	plan, attempt := testPilotPlan(), testConductorAttempt()
	for _, attack := range []string{"zero", "missing", "duplicate", "unknown", "skipped", "expected-failed", "failed", "reordered", "wrong-execution", "wrong-attempt", "wrong-inventory", "guest-marker", "truncated-frame", "trailing-event", "duplicate-json-field", "oversize"} {
		t.Run(attack, func(t *testing.T) {
			events := testConductorEvents(plan, attempt)
			switch attack {
			case "zero":
				events = nil
			case "missing":
				events = events[:len(events)-1]
			case "duplicate":
				events[3].Case = events[1].Case
			case "unknown":
				events[1].Case = "candidate-chosen-case"
			case "skipped", "expected-failed", "failed":
				events[1].Phase = attack
			case "reordered":
				events[0], events[1] = events[1], events[0]
			case "wrong-execution":
				events[0].Execution = strings.Repeat("0", 64)
			case "wrong-attempt":
				events[0].Attempt = strings.Repeat("0", 64)
			case "wrong-inventory":
				events[0].Inventory = strings.Repeat("0", 64)
			case "trailing-event":
				events = append(events, events[len(events)-1])
			}
			body := encodeConductorEvents(t, events)
			switch attack {
			case "guest-marker":
				body = append([]byte("PASS exitCode=0\n"), body...)
			case "truncated-frame":
				body = body[:len(body)-1]
			case "duplicate-json-field":
				body = bytes.Replace(body, []byte(`"version":1`), []byte(`"version":0,"version":1`), 1)
			case "oversize":
				body = []byte(strings.Repeat("x", MaxConductorEventBytes+1) + "\n")
			}
			if _, err := ReadConductorResults(bytes.NewReader(body), plan, attempt); err == nil {
				t.Fatalf("accepted %s stream", attack)
			}
		})
	}
}

func TestPilotPlanAndResourceRemainClosed(t *testing.T) {
	for _, attack := range []string{"profile", "retired-profile", "mutable-conductor", "same-namespace", "server-namespace", "artifact-size", "kernel-digest", "resources"} {
		t.Run(attack, func(t *testing.T) {
			plan := testPilotPlan()
			switch attack {
			case "profile":
				plan.Profile = "candidate-shell"
			case "retired-profile":
				plan.Profile = "beacon-transport-amd64-v1"
			case "mutable-conductor":
				plan.ConductorImageRef = "registry.example/conductor:latest"
			case "same-namespace":
				plan.ConductorNamespace = plan.GuestNamespace
			case "server-namespace":
				plan.GuestNamespace = plan.ServerNamespace
			case "artifact-size":
				plan.ArtifactBytes = MaxBeaconArtifactBytes + 1
			case "kernel-digest":
				plan.KernelDigest = "latest"
			case "resources":
				plan.Spec.Resources.CPUCores++
			}
			if err := ValidatePilotPlan(plan); err == nil {
				t.Fatalf("accepted %s plan", attack)
			}
		})
	}
	plan := testPilotPlan()
	for _, kind := range []string{"Secret", "ConfigMap", "Namespace", "PersistentVolumeClaim"} {
		intent := ResourceIntent{Key: "candidate", Kind: kind, Namespace: plan.GuestNamespace, Name: "candidate", SpecIdentity: strings.Repeat("a", 64)}
		if err := ValidatePilotResource(plan, intent); err == nil {
			t.Fatalf("accepted unapproved resource %s", kind)
		}
	}
}
