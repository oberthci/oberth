package vmrunner

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestProtectedExecutionWireCompatibility(t *testing.T) {
	body, err := os.ReadFile("testdata/protected-intent-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var i KVMIntent
	if err = json.Unmarshal(body, &i); err != nil {
		t.Fatal(err)
	}
	d := KVMIntentDigest(i)
	r := KVMReceipt{IntentDigest: d, WorkflowUID: "workflow", Binding: KVMTerminationBinding{Namespace: i.Namespace, WorkflowName: i.WorkflowName, WorkflowUID: "workflow", PodName: "pod", PodUID: "pod-uid", ContainerID: "containerd://id", HarnessImageRef: i.Recipe.HarnessImage, AdmittedAt: i.AdmittedAt, Deadline: i.Deadline}, Report: KVMReport{Version: 1, IntentDigest: d, InventoryDigest: i.InventoryDigest, ArtifactSetDigest: strings.Repeat("b", 64), CaseCount: i.CaseCount}, Termination: KVMTerminationRecord{ImageID: i.Recipe.HarnessImage, ContainerID: "containerd://id", PodUID: "pod-uid", WorkflowUID: "workflow", StartedAt: i.AdmittedAt, FinishedAt: i.AdmittedAt}, FinishedAt: i.AdmittedAt}
	if err = r.Validate(i); err != nil {
		t.Fatal(err)
	}
	e := KVMExecution{Intent: i, WorkflowUID: r.WorkflowUID, Submitted: true, Cleaned: true, Receipt: &r}
	refusal := KVMRefusal{Code: "deadline", ObservedAt: i.Deadline, Termination: &r.Termination}
	wire, err := json.Marshal(struct {
		Execution KVMExecution
		Refusal   KVMRefusal
	}{e, refusal})
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprintf("%x", sha256.Sum256(wire))
	if got != "2a49de6706a2972060a751d5ff0dabd05ed1378fa10a7db1f99635b5364ea316" {
		t.Fatalf("generic execution/refusal wire digest changed: %s", got)
	}
}
