package argoworkflow

import (
	"encoding/json"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func capabilityPolicyForTest(t *testing.T) Policy {
	t.Helper()
	var policy Policy
	if err := json.Unmarshal([]byte(`{"Capability":{"namespace":"pipelines","service_account":"operator-executor","resource_kinds":[{"api_version":"tasks.example.test/v1","kind":"Task","actions":["create","get","delete"]}],"extended_resources":{"devices.example.test/worker":2}}}`), &policy); err != nil {
		t.Fatal(err)
	}
	return policy
}

const capabilityResourceDocument = `apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 300
  templates:
    - name: main
      resource:
        action: create
        setOwnerReference: true
        manifest: |
          apiVersion: tasks.example.test/v1
          kind: Task
          metadata:
            name: '{{workflow.name}}-task'
            namespace: pipelines
          spec:
            operatorDefinedPayload: opaque
`

func TestOperatorCapabilityAdmitsOnlyGrantedResourceEnvelopes(t *testing.T) {
	policy := capabilityPolicyForTest(t)
	mustAdmit(t, capabilityResourceDocument, policy)
	cases := []struct {
		name, document string
		policy         Policy
	}{
		{"ungranted", capabilityResourceDocument, Policy{}},
		{"foreign namespace", strings.ReplaceAll(capabilityResourceDocument, "namespace: pipelines", "namespace: other"), policy},
		{"foreign kind", strings.ReplaceAll(capabilityResourceDocument, "kind: Task", "kind: Other"), policy},
		{"dynamic kind", strings.ReplaceAll(capabilityResourceDocument, "kind: Task", "kind: '{{inputs.parameters.kind}}'"), policy},
		{"ungranted action", strings.ReplaceAll(capabilityResourceDocument, "action: create", "action: patch"), policy},
		{"multiple objects", capabilityResourceDocument + "          ---\n          apiVersion: v1\n          kind: Secret\n", policy},
		{"arbitrary flags", strings.ReplaceAll(capabilityResourceDocument, "action: create", "action: create\n        flags: [--namespace=other]"), policy},
		{"parameter injection", strings.ReplaceAll(capabilityResourceDocument, "operatorDefinedPayload: opaque", "operatorDefinedPayload: '{{inputs.parameters.payload}}'"), policy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf, err := Decode([]byte(tc.document))
			if err == nil {
				err = Admit(wf, tc.policy)
			}
			if err == nil {
				t.Fatal("ungranted resource envelope admitted")
			}
		})
	}
}

func TestOperatorCapabilityExtendedResourcesAreFiniteIntegers(t *testing.T) {
	document := admissibleDocument + `        resources:
          requests: {cpu: '1', memory: 128Mi, devices.example.test/worker: '1'}
          limits: {memory: 128Mi, devices.example.test/worker: '1'}
`
	policy := capabilityPolicyForTest(t)
	mustAdmit(t, document, policy)
	for _, bad := range []string{"0", "-1", "0.5", "3", "{{inputs.parameters.count}}"} {
		t.Run(bad, func(t *testing.T) {
			body := strings.ReplaceAll(document, "worker: '1'", "worker: '"+bad+"'")
			wf, err := Decode([]byte(body))
			if err == nil {
				err = Admit(wf, policy)
			}
			if err == nil {
				t.Fatal("invalid extended resource quantity admitted")
			}
		})
	}
	wf, err := Decode([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	if err = Admit(wf, Policy{}); err == nil {
		t.Fatal("extended resource admitted without a grant")
	}
}

func TestOperatorResourceExecutorRequiresNamedTemplate(t *testing.T) {
	for _, location := range []string{"inline", "defaults"} {
		t.Run(location, func(t *testing.T) {
			wf, err := Decode([]byte(capabilityResourceDocument))
			if err != nil {
				t.Fatal(err)
			}
			resource := wf.Spec.Templates[0].DeepCopy()
			if location == "inline" {
				wf.Spec.Templates = []wfv1.Template{{Name: "main", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "task", Inline: resource}}}}}}
			} else {
				wf.Spec.TemplateDefaults = resource
			}
			if err := Admit(wf, capabilityPolicyForTest(t)); err == nil {
				t.Fatal("resource identity has no named executor binding")
			}
		})
	}
}
