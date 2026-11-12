package argojob

import (
	"encoding/json"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

const operatorResourceDocument = `apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    example.com/tier: L
spec:
  entrypoint: main
  activeDeadlineSeconds: 300
  serviceAccountName: author-chosen
  templates:
  - name: main
    resource:
      action: create
      manifest: |
        apiVersion: tasks.example.test/v1
        kind: Task
        metadata:
          name: '{{workflow.name}}-task'
          namespace: oberth-argo
        spec:
          payload: opaque
`

func operatorCapability() *argoworkflow.Capability {
	return &argoworkflow.Capability{
		Namespace: testNamespace, ServiceAccount: "operator-task-runner",
		ResourceKinds:     []argoworkflow.ResourceKindCapability{{APIVersion: "tasks.example.test/v1", Kind: "Task", Actions: []string{"create"}}},
		ExtendedResources: map[string]int64{"devices.example.test/worker": 2},
	}
}

func requestWithCapability(t *testing.T, source string, capability *argoworkflow.Capability) Request {
	t.Helper()
	r := testRequest(pipeline.TriggerCI, source)
	// Decode the new request field so this regression also executes against
	// the pre-capability request type instead of failing to compile there.
	body, err := json.Marshal(map[string]any{"Capability": capability})
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCapabilityBuildBindsOperatorIdentityAndRefusesEscapes(t *testing.T) {
	c := operatorCapability()
	r := requestWithCapability(t, operatorResourceDocument, c)
	wf, err := Build(testConfig(), r)
	if err != nil {
		t.Fatalf("granted resource workflow: %v", err)
	}
	if wf.Spec.ServiceAccountName != testPipelineAcct || wf.Spec.AutomountServiceAccountToken == nil || *wf.Spec.AutomountServiceAccountToken || wf.Namespace != c.Namespace || wf.Spec.Templates[0].Executor == nil || wf.Spec.Templates[0].Executor.ServiceAccountName != c.ServiceAccount {
		t.Fatalf("operator identity not bound: %+v", wf.Spec)
	}
	if wf.Annotations["oberth.ci/workflow-capability"] == "" {
		t.Fatal("submission lacks a server-owned capability binding")
	}
	for _, tc := range []struct{ name, account, namespace string }{
		{"foreign namespace", c.ServiceAccount, "other"},
		{"shared executor", testExecutorAcct, testNamespace},
		{"shared pipeline", testPipelineAcct, testNamespace},
		{"shared secret identity", testCredentialedAcct, testNamespace},
		{"shared CI identity", testCISecretsAcct, testNamespace},
		{"per-repository secret identity", "other-repo-secret", testNamespace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			grant := operatorCapability()
			grant.ServiceAccount, grant.Namespace = tc.account, tc.namespace
			config := testConfig()
			config.PerRepoIdentities = map[string]PerRepoIdentityConfig{"other/repo": {ServiceAccountName: "other-repo-secret"}}
			if _, err := Build(config, requestWithCapability(t, operatorResourceDocument, grant)); err == nil {
				t.Fatal("unsafe operator identity accepted")
			}
		})
	}
	if _, err = Build(testConfig(), requestWithCapability(t, operatorResourceDocument, nil)); err == nil {
		t.Fatal("ungranted resource workflow accepted")
	}
	secret := "oberth/upstream/acme/oberth/test-secret"
	r = requestWithCapability(t, strings.Replace(operatorResourceDocument, "example.com/tier: L", "example.com/tier: L\n    oberth.ci/secret-paths: "+secret, 1), c)
	r.ApprovedSecrets[secret] = true
	if _, err := Build(testConfig(), r); err == nil {
		t.Fatal("operator authority combined with secret access")
	}
	ordinary, err := Build(testConfig(), requestWithCapability(t, greedyDocument, c))
	if err != nil || ordinary.Spec.ServiceAccountName != testPipelineAcct || ordinary.Annotations["oberth.ci/workflow-capability"] != "" {
		t.Fatalf("unused grant changed ordinary CI: %v", err)
	}
}

func TestCapabilityBuildRequiresOperatorGrantWithoutGlobalNumericReservation(t *testing.T) {
	source := greedyDocument + `        resources:
          requests: {cpu: '1', memory: 128Mi, devices.example.test/worker: '1'}
          limits: {memory: 128Mi, devices.example.test/worker: '1'}
`
	r := requestWithCapability(t, source, operatorCapability())
	if _, err := Build(testConfig(), r); err != nil {
		t.Fatalf("reserved extended resources: %v", err)
	}
	if _, err := Build(testConfig(), r); err != nil {
		t.Fatal("authorized device requests incorrectly required a global numeric reservation", err)
	}
	// CPU/memory declarations are optional; device authorization remains bounded.
	r = requestWithCapability(t, strings.Replace(source, "cpu: '1', memory: 128Mi, ", "", 1), operatorCapability())
	if _, err := Build(testConfig(), r); err != nil {
		t.Fatal("optional CPU/memory declarations were required", err)
	}
}

func TestCapabilityResourceKeepsOperatorLifecycle(t *testing.T) {
	r := requestWithCapability(t, operatorResourceDocument, operatorCapability())
	wf, err := Build(testConfig(), r)
	if err != nil {
		t.Fatal(err)
	}
	for _, template := range wf.Spec.Templates {
		if template.Resource != nil && strings.Contains(template.PodSpecPatch, `"command":["argoexec","resource","get"]`) {
			t.Error("generic create was rewritten as a legacy broker get")
		}
	}
}

// Replay the captured controller Pod boundary with today's Build output. A
// sibling resource must never change a container or script's ambient identity.
func TestCapabilityKeepsOrdinaryControllerPodsTokenless(t *testing.T) {
	for _, shape := range []string{"container", "script"} {
		t.Run(shape, func(t *testing.T) {
			wf, err := argoworkflow.Decode([]byte(nonrootTestDocument))
			if err != nil {
				t.Fatal(err)
			}
			resource, err := argoworkflow.Decode([]byte(operatorResourceDocument))
			if err != nil {
				t.Fatal(err)
			}
			wf.Spec.Templates = append(wf.Spec.Templates, resource.Spec.Templates[0])
			if shape == "script" {
				leaf := &wf.Spec.Templates[0]
				leaf.Script = &wfv1.ScriptTemplate{Container: *leaf.Container, Source: "true"}
				leaf.Script.Command = []string{"/bin/sh"}
				leaf.Container = nil
			}
			encoded, err := json.Marshal(wf)
			if err != nil {
				t.Fatal(err)
			}
			config := testConfig()
			config.NonrootProfile = argoworkflow.NonrootStaticProfile
			r := requestWithCapability(t, string(encoded), operatorCapability())
			r.SourceVolume = SourceVolume{ClaimName: "run-source", SubPath: "src", ArtifactsSubPath: "artifacts"}
			built, err := Build(config, nonrootFixtureProof(r))
			if err != nil {
				t.Fatal(err)
			}
			pod := nonrootApplyControllerFixture(t, nonrootControllerFixture(t, shape+"-before-patch.json"), built)
			if pod.Spec.ServiceAccountName != testPipelineAcct || pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
				t.Errorf("ordinary Pod received operator identity or ambient token: account=%s automount=%v", pod.Spec.ServiceAccountName, pod.Spec.AutomountServiceAccountToken)
			}
			assertWIFPodIsolation(t, "sibling", pod.Spec)
			for _, tmpl := range built.Spec.Templates {
				want := config.ExecutorServiceAccount
				if tmpl.Resource != nil {
					want = r.Capability.ServiceAccount
				}
				if tmpl.Executor != nil && tmpl.Executor.ServiceAccountName != want {
					t.Errorf("%s executor=%s, want %s", tmpl.Name, tmpl.Executor.ServiceAccountName, want)
				}
			}
		})
	}
}
