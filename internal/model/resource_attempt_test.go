package model

import "testing"

func TestAttemptInventoryDoesNotRequireOrInventPodIdentity(t *testing.T) {
	i := ResourceAttemptInventory{RunID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", WorkflowName: "wf", WorkflowUID: "11111111-1111-4111-8111-111111111111", Namespace: "argo", Complete: true, Terminal: true, Attempts: []ResourceAttempt{{NodeID: "wf-1", Stage: "build", Step: "compile", Template: "compile", Status: StepFailed}}}
	if i.Validate() != nil || i.Attempts[0].PodUID != "" {
		t.Fatal("missing Pod requires a fabricated UID")
	}
	for _, kind := range []string{"pod", "workflow", "duplicate", "status"} {
		bad := i
		bad.Attempts = append([]ResourceAttempt{}, i.Attempts...)
		switch kind {
		case "pod":
			bad.Attempts[0].PodUID = "unknown"
		case "workflow":
			bad.WorkflowUID = "unknown"
		case "duplicate":
			bad.Attempts = append(bad.Attempts, bad.Attempts[0])
		case "status":
			bad.Attempts[0].Status = "OOMKilled"
		}
		if bad.Validate() == nil {
			t.Fatalf("invalid %s binding accepted", kind)
		}
	}
}
