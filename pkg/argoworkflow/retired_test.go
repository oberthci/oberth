package argoworkflow

import (
	"errors"
	"strings"
	"testing"
)

func TestRetiredDeclarationsRequireWorkflowCapability(t *testing.T) {
	for _, policy := range []Policy{{}, capabilityPolicyForTest(t)} {
		for _, annotation := range []string{"oberth.ci/kvm-templates", "oberth.ci/guest-test-templates", "oberth.ci/trusted-tests-entrypoint", "oberth.ci/vmi-quota"} {
			wf, err := Decode([]byte(admissibleDocument))
			if err != nil {
				t.Fatal(err)
			}
			wf.Annotations = map[string]string{annotation: "main"}
			if err := Admit(wf, policy); !errors.Is(err, ErrWorkflowCapabilityRequired) {
				t.Fatalf("%s: %v", annotation, err)
			}
		}
	}
	resource := strings.ReplaceAll(strings.ReplaceAll(capabilityResourceDocument, "tasks.example.test/v1", "kubevirt.io/v1"), "kind: Task", "kind: VirtualMachineInstance")
	wf, err := Decode([]byte(resource))
	if err != nil {
		t.Fatal(err)
	}
	if err := Admit(wf, Policy{}); !errors.Is(err, ErrWorkflowCapabilityRequired) {
		t.Fatal(err)
	}
	// The envelope is deliberately opaque: the generic allowlist can authorize
	// this kind just like any other installed, namespaced API.
	policy := capabilityPolicyForTest(t)
	policy.Capability.ResourceKinds[0].APIVersion = "kubevirt.io/v1"
	policy.Capability.ResourceKinds[0].Kind = "VirtualMachineInstance"
	if err := Admit(wf, policy); err != nil {
		t.Fatal(err)
	}
	device := admissibleDocument + `        resources:
          requests: {cpu: '1', memory: 128Mi, devices.example.test/worker: '1'}
          limits: {memory: 128Mi, devices.example.test/worker: '1'}
`
	wf, err = Decode([]byte(device))
	if err != nil {
		t.Fatal(err)
	}
	if err := Admit(wf, Policy{}); !errors.Is(err, ErrWorkflowCapabilityRequired) {
		t.Fatal(err)
	}
	if err := Admit(wf, capabilityPolicyForTest(t)); err != nil {
		t.Fatal(err)
	}
}
