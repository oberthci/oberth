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
		{"control-plane priority", strings.ReplaceAll(capabilityResourceDocument, "operatorDefinedPayload: opaque", "priorityClassName: control-oberth-control-plane"), policy},
		{"embedded Pod priority", strings.ReplaceAll(capabilityResourceDocument, "operatorDefinedPayload: opaque", "template:\n              spec:\n                priorityClassName: control-oberth-control-plane"), policy},
		{"embedded Pod patch", strings.ReplaceAll(capabilityResourceDocument, "operatorDefinedPayload: opaque", `podSpecPatch: '{"priorityClassName":"control-oberth-control-plane"}'`), policy},
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

func TestCapabilityIdentitySeparatesResourceAuthority(t *testing.T) {
	policy := capabilityPolicyForTest(t)
	identity := testBranchIdentity()
	identity.Namespace = policy.Capability.Namespace
	identity.ResourceServiceAccountName = policy.Capability.ServiceAccount
	for _, scenario := range []string{"valid", "pipeline", "executor", "automount", "missing binding", "foreign binding"} {
		t.Run(scenario, func(t *testing.T) {
			candidate := identity
			switch scenario {
			case "pipeline":
				candidate.ServiceAccountName = candidate.ResourceServiceAccountName
			case "executor":
				candidate.ExecutorServiceAccountName = candidate.ResourceServiceAccountName
			case "automount":
				candidate.AutomountServiceAccountToken = true
			case "missing binding":
				candidate.ResourceServiceAccountName = ""
			case "foreign binding":
				candidate.ResourceServiceAccountName = "other-operator"
			}
			wf, err := Decode([]byte(capabilityResourceDocument))
			if err != nil {
				t.Fatal(err)
			}
			err = Prepare(wf, policy, candidate)
			if (err == nil) != (scenario == "valid") {
				t.Fatalf("Prepare: %v", err)
			}
		})
	}
}

func TestCapabilityResourceEnvelopeRejectsAmbiguousKeys(t *testing.T) {
	for _, test := range []struct{ name, manifest string }{
		{"unicode kind", "apiVersion: tasks.example.test/v1\nkind: Pod\nKind: Task\nmetadata: {name: x, namespace: pipelines}"},
		{"unicode namespace", "apiVersion: tasks.example.test/v1\nkind: Task\nmetadata: {name: x, namespace: kube-system, nameſpace: pipelines}"},
		{"ascii kind", "apiVersion: tasks.example.test/v1\nKind: Task\nmetadata: {name: x}"},
		{"ascii apiVersion", "APIVersion: tasks.example.test/v1\nkind: Task\nmetadata: {name: x}"},
		{"ascii metadata", "apiVersion: tasks.example.test/v1\nkind: Task\nMetadata: {name: x}"},
		{"ascii namespace", "apiVersion: tasks.example.test/v1\nkind: Task\nmetadata: {name: x, Namespace: pipelines}"},
		{"ascii name", "apiVersion: tasks.example.test/v1\nkind: Task\nmetadata: {Name: x}"},
		{"ascii generateName", "apiVersion: tasks.example.test/v1\nkind: Task\nmetadata: {GenerateName: x}"},
		{"yaml duplicate", "apiVersion: tasks.example.test/v1\nkind: Pod\nkind: Task\nmetadata: {name: x}"},
		{"json duplicate", `{"apiVersion":"tasks.example.test/v1","kind":"Pod","kind":"Task","metadata":{"name":"x"}}`},
		{"json nested duplicate", `{"apiVersion":"tasks.example.test/v1","kind":"Task","metadata":{"name":"x","namespace":"other","namespace":"pipelines"}}`},
		{"opaque duplicate", `{"apiVersion":"tasks.example.test/v1","kind":"Task","metadata":{"name":"x"},"spec":{"duplicate":1,"duplicate":2}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			resource := &wfv1.ResourceTemplate{Action: "create", Manifest: test.manifest}
			if err := admitCapabilityResource("test", resource, capabilityPolicyForTest(t).Capability); err == nil {
				t.Fatal("ambiguous resource envelope accepted")
			}
		})
	}
	for _, manifest := range []string{
		"apiVersion: tasks.example.test/v1\nkind: Task\nmetadata: {name: x, namespace: pipelines}\nspec: {PascalCase: opaque, metadata: {Name: opaque}}",
		`{"apiVersion":"tasks.example.test/v1","kind":"Task","metadata":{"generateName":"x-"},"spec":{"PascalCase":"opaque"}}`,
	} {
		if err := admitCapabilityResource("test", &wfv1.ResourceTemplate{Action: "create", Manifest: manifest}, capabilityPolicyForTest(t).Capability); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCapabilityRefusesResourceTemplatesFromPinnedFragments(t *testing.T) {
	for _, name := range []string{"frag-deadbeef-task", "frag-0123456789abcdef-main", "local-task"} {
		t.Run(name, func(t *testing.T) {
			wf, err := Decode([]byte(capabilityResourceDocument))
			if err != nil {
				t.Fatal(err)
			}
			wf.Spec.Templates[0].Name = name
			wf.Spec.Entrypoint = name
			err = Admit(wf, capabilityPolicyForTest(t))
			if strings.HasPrefix(name, "frag-") {
				if err == nil || !strings.Contains(err.Error(), "consuming repository") {
					t.Fatalf("foreign resource inherited consumer authority: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
