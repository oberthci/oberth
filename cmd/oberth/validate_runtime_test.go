package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// Exercise the CLI and the server's actual Build entrypoint with identical
// documents. Approved paths and identities are fixture data, so a refusal must
// come from document admission rather than unavailable server state.
func TestValidateRuntimeAdmissionParity(t *testing.T) {
	const secretPath = "oberth/upstream/acme/project/ci-secret"
	const document = `apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    example.com/tier: S
spec:
  entrypoint: unit
  activeDeadlineSeconds: 600
  templates:
    - name: unit
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`
	base, err := argoworkflow.Decode([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	credential := func(wf *wfv1.Workflow) {
		wf.Annotations[argoworkflow.SecretPathsAnnotation] = secretPath
		wf.Spec.Templates[0].Container.Command = []string{"oberth", "secretstore", "exec", "--path=" + secretPath, "--", "/bin/true"}
	}
	mounts := func(wf *wfv1.Workflow, mode string) {
		wf.Spec.Templates[0].Metadata.Annotations = map[string]string{argoworkflow.WorkspaceMountsAnnotation: mode}
	}
	trivy := func(wf *wfv1.Workflow, mode, cache, depends string, inline bool) {
		leaf := wf.Spec.Templates[0].DeepCopy()
		leaf.Name = "scan"
		leaf.Metadata.Annotations = map[string]string{argoworkflow.WorkspaceMountsAnnotation: mode}
		leaf.Container.Command = []string{"trivy", "fs", "--cache-dir", cache, "."}
		a := wfv1.DAGTask{Name: "scan-a", Template: leaf.Name}
		b := wfv1.DAGTask{Name: "scan-b", Template: leaf.Name, Depends: depends}
		if inline {
			a.Template, b.Template = "", ""
			a.Inline, b.Inline = leaf.DeepCopy(), leaf.DeepCopy()
		}
		wf.Spec.Templates = []wfv1.Template{{Name: "unit", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{a, b}}}}
		if !inline {
			wf.Spec.Templates = append(wf.Spec.Templates, *leaf)
		}
	}
	for _, tc := range []struct {
		name    string
		trigger pipeline.Trigger
		mutate  func(*wfv1.Workflow)
		refusal string
	}{
		{"server-waiter-name", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			wf.Spec.Templates[0].InitContainers = []wfv1.UserContainer{{Container: corev1.Container{Name: "oberth-executor-token", Image: wf.Spec.Templates[0].Container.Image, Env: []corev1.EnvVar{{Name: "LD_PRELOAD", Value: "/loot/x.so"}}}}}
		}, "reserved server-owned oberth- prefix"},
		{"executor-sidecar-name", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			wf.Spec.Templates[0].Sidecars = []wfv1.UserContainer{{Container: corev1.Container{Name: "wait", Image: wf.Spec.Templates[0].Container.Image}}}
		}, "reserved for the Argo executor"},
		{"executor-init-name", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			wf.Spec.Templates[0].InitContainers = []wfv1.UserContainer{{Container: corev1.Container{Name: "main", Image: wf.Spec.Templates[0].Container.Image}}}
		}, "reserved for the Argo executor"},
		{"executor-token-mount-name", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			wf.Spec.Templates[0].Container.VolumeMounts = []corev1.VolumeMount{{Name: argoworkflow.ExecutorIdentityVolumeName, MountPath: "/identity"}}
		}, "reserved executor identity volume"},
		{"executor-token-mount-path", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			wf.Spec.Templates[0].Container.VolumeMounts = []corev1.VolumeMount{{Name: "scratch", MountPath: argoworkflow.ExecutorIdentityMountPath}}
		}, "reserved server helper path"},
		{"templated-env-name", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			wf.Spec.Templates[0].Container.Env = []corev1.EnvVar{{Name: "{{='ARGO_'+'PROGRESS_PATCH_TICK_DURATION'}}", Value: "0"}}
		}, "environment names must be literal"},
		{"templated-env-value", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			wf.Spec.Templates[0].Container.Name = "main"
			wf.Spec.Templates[0].Container.Env = []corev1.EnvVar{{Name: "CUSTOM", Value: "{{workflow.name}}"}}
		}, ""},
		{"plain", pipeline.TriggerCI, func(*wfv1.Workflow) {}, ""},
		{"tokenless-cache", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			mounts(wf, "cache")
			wf.Spec.Templates[0].AutomountServiceAccountToken = ptr.To(false)
		}, ""},
		{"credentialed-ci-cache", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			credential(wf)
			mounts(wf, "cache")
		}, "cannot use oberth.ci/workspace-mounts: cache on the CI tier"},
		{"credentialed-ci-none", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			credential(wf)
			mounts(wf, "none")
		}, ""},
		{"credentialed-release-cache", pipeline.TriggerRelease, func(wf *wfv1.Workflow) {
			credential(wf)
			mounts(wf, "cache")
		}, ""},
		{"tokenless-credential-chain", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			credential(wf)
			wf.Spec.Templates[0].AutomountServiceAccountToken = ptr.To(false)
		}, "disables its service-account token but invokes a credential chain"},
		{"workflow-token-opt-out", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			credential(wf)
			wf.Spec.AutomountServiceAccountToken = ptr.To(false)
			wf.Spec.Templates[0].AutomountServiceAccountToken = ptr.To(true)
		}, "disables its service-account token but invokes a credential chain"},
		{"defaults-token-opt-out", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			credential(wf)
			wf.Spec.TemplateDefaults = &wfv1.Template{AutomountServiceAccountToken: ptr.To(false)}
			wf.Spec.Templates[0].AutomountServiceAccountToken = ptr.To(true)
		}, "disables its service-account token but invokes a credential chain"},
		{"inline-parent-token-opt-out", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			credential(wf)
			leaf := wf.Spec.Templates[0].DeepCopy()
			leaf.AutomountServiceAccountToken = ptr.To(true)
			wf.Spec.Templates = []wfv1.Template{{Name: "unit", AutomountServiceAccountToken: ptr.To(false),
				DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "exec", Inline: leaf}}}}}
		}, "disables its service-account token but invokes a credential chain"},
		{"script-credentialed-cache", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			credential(wf)
			mounts(wf, "cache")
			leaf := &wf.Spec.Templates[0]
			leaf.Script = &wfv1.ScriptTemplate{Container: *leaf.Container, Source: "true"}
			leaf.Container = nil
		}, "cannot use oberth.ci/workspace-mounts: cache on the CI tier"},
		{"containerset-isolation-with-defaults", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			mounts(wf, "cache")
			wf.Spec.TemplateDefaults = &wfv1.Template{}
			leaf := &wf.Spec.Templates[0]
			leaf.Container.Name = "main"
			leaf.ContainerSet = &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: *leaf.Container}}}
			leaf.Container = nil
		}, "requests workspace isolation with templateDefaults"},
		{"isolation-with-defaults", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			mounts(wf, "none")
			wf.Spec.TemplateDefaults = &wfv1.Template{}
		}, "requests workspace isolation with templateDefaults"},
		{"controls-on-dag", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			trivy(wf, "none", "/tmp/trivy", "", false)
			mounts(wf, "none")
		}, "must declare workspace controls on each Pod leaf"},
		{"controls-on-defaults", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			wf.Spec.TemplateDefaults = &wfv1.Template{Metadata: wfv1.Metadata{Annotations: map[string]string{argoworkflow.WorkspaceMountsAnnotation: "none"}}}
		}, "workspace isolation must be declared on templates, not templateDefaults"},
		{"mounts-on-defaults", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			wf.Spec.TemplateDefaults = &wfv1.Template{Container: wf.Spec.Templates[0].Container.DeepCopy()}
			wf.Spec.TemplateDefaults.Container.VolumeMounts = []corev1.VolumeMount{{Name: "work", MountPath: "/work"}}
		}, "templateDefaults volumeMounts or volumeDevices are not admitted"},
		{"trivy-unordered-shared", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			trivy(wf, "cache", argojob.CacheMountPath+"/trivy", "", false)
		}, ""},
		{"trivy-unordered-inline", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			trivy(wf, "cache", argojob.CacheMountPath+"/trivy", "", true)
		}, ""},
		{"trivy-ordered-shared", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			trivy(wf, "cache", argojob.CacheMountPath+"/trivy", "scan-a.Succeeded", false)
		}, ""},
		{"trivy-private-cache", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			trivy(wf, "cache", "/tmp/trivy", "", false)
		}, ""},
		{"trivy-no-workspace", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			trivy(wf, "none", argojob.CacheMountPath+"/trivy", "", false)
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wf := base.DeepCopy()
			tc.mutate(wf)
			assertValidateRuntimeParity(t, document, secretPath, tc.trigger, wf, tc.refusal)
		})
	}
}

// Build the server request and the CLI consumer from one mutated Workflow and
// require both entrypoints to agree on acceptance or on the exact refusal.
func assertValidateRuntimeParity(t *testing.T, document, secretPath string, trigger pipeline.Trigger, wf *wfv1.Workflow, refusal string) {
	t.Helper()
	source, err := json.Marshal(wf)
	if err != nil {
		t.Fatal(err)
	}
	config := argojob.Config{
		Namespace: "pipelines", PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "release",
		CISecretsServiceAccount: "ci-secrets", ExecutorServiceAccount: "executor",
		VaultAddress: "https://vault.example.com", VaultCredentialedRole: "release", VaultCISecretsRole: "ci-secrets",
		PerRepoIdentities:   map[string]argojob.PerRepoIdentityConfig{"github/acme/project": {ServiceAccountName: "project-release"}},
		PerRepoCIIdentities: map[string]argojob.PerRepoIdentityConfig{"github/acme/project": {ServiceAccountName: "project-ci"}},
		PerStepIdentities: map[string]argojob.PerRepoIdentityConfig{
			"github/acme/project/.oberth/release.yaml/*": {ServiceAccountName: "project-release-step"},
			"github/acme/project/.oberth/build.yaml/*":   {CIServiceAccountName: "project-ci-step"},
		},
	}
	request := argojob.Request{
		RunID: "parity", RepositoryID: 1, Name: "parity", Repo: "project", UpstreamName: "github", UpstreamOrg: "acme",
		Ref: "refs/heads/main", SHA: strings.Repeat("a", 40), Trigger: trigger, Source: source,
		ApprovedSecrets: map[string]bool{secretPath: true},
		StepGrants:      map[string]map[string]bool{secretPath: {"*": true}},
		SourceVolume:    argojob.SourceVolume{ClaimName: "source", SubPath: "src", BinarySubPath: "bin"},
	}
	root := writeConsumer(t, document)
	file := "build.yaml"
	if trigger == pipeline.TriggerRelease {
		file = "release.yaml"
		request.Ref = "refs/tags/v1.0.0"
	}
	if err := os.WriteFile(filepath.Join(root, ".oberth", file), source, 0o600); err != nil {
		t.Fatal(err)
	}
	_, serverErr := argojob.Build(config, request)
	var output bytes.Buffer
	cliErr := runValidate(context.Background(), []string{root}, &output)
	if refusal == "" {
		if serverErr != nil || cliErr != nil || !strings.Contains(output.String(), "result: PASS") {
			t.Fatalf("valid control: server=%v, validate=%v\n%s", serverErr, cliErr, output.String())
		}
		return
	}
	if serverErr == nil || !strings.Contains(serverErr.Error(), refusal) {
		t.Fatalf("server refusal = %v, want %q", serverErr, refusal)
	}
	if cliErr == nil || !strings.Contains(output.String(), "result: FAIL") || strings.Contains(output.String(), "result: PASS") {
		t.Fatalf("server refused %v; validate must also refuse: %v\n%s", serverErr, cliErr, output.String())
	}
	if !strings.Contains(output.String(), serverErr.Error()) {
		t.Fatalf("validate did not report the identical server refusal %q:\n%s", serverErr, output.String())
	}
}

// Native allowances and ownership refusals use the same server and CLI boundary.
func TestValidateRefusalRuntimeParity(t *testing.T) {
	const document = `apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: unit
  templates:
  - name: unit
    container:
      image: registry.example.test/runner:1
      command: [/bin/true]
`
	for _, tc := range []struct {
		name    string
		trigger pipeline.Trigger
		mutate  func(*wfv1.Workflow)
		refusal string
	}{
		{"native-image-and-pull-secrets", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			wf.Spec.Templates[0].Container.Image = "registry.example.test/runner:latest"
			wf.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "registry"}}
			wf.GenerateName = "author-"
		}, ""},
		{"native-resource-env", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			wf.Spec.Templates[0].Container.Env = []corev1.EnvVar{{Name: "CPU", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "limits.cpu"}}}}
		}, ""},
		{"workflow-owned-label", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			wf.Labels = map[string]string{"oberth.ci/run": "forged"}
		}, "metadata.labels"},
		{"workflow-owned-annotation", pipeline.TriggerCI, func(wf *wfv1.Workflow) {
			wf.Annotations["workflows.argoproj.io/controller-instanceid"] = "forged"
		}, "metadata.annotations"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wf, err := argoworkflow.Decode([]byte(document))
			if err != nil {
				t.Fatal(err)
			}
			wf.Annotations = map[string]string{}
			tc.mutate(wf)
			source, err := json.Marshal(wf)
			if err != nil {
				t.Fatal(err)
			}
			config := argojob.Config{Namespace: "pipelines", PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "release", CISecretsServiceAccount: "ci-secrets", ExecutorServiceAccount: "executor"}
			request := argojob.Request{RunID: "parity", RepositoryID: 1, Name: "parity", Repo: "project", UpstreamName: "upstream", UpstreamOrg: "example", Ref: "refs/heads/main", SHA: strings.Repeat("a", 40), Trigger: tc.trigger, Source: source, ApprovedSecrets: map[string]bool{}, SourceVolume: argojob.SourceVolume{ClaimName: "source", SubPath: "src", BinarySubPath: "bin"}}
			root := writeConsumer(t, document)
			if err := os.WriteFile(filepath.Join(root, ".oberth", "build.yaml"), source, 0600); err != nil {
				t.Fatal(err)
			}
			_, serverErr := argojob.Build(config, request)
			var output bytes.Buffer
			cliErr := runValidate(context.Background(), []string{root}, &output)
			if tc.refusal == "" {
				if serverErr != nil || cliErr != nil {
					t.Fatalf("server=%v CLI=%v: %s", serverErr, cliErr, output.String())
				}
			} else if serverErr == nil || cliErr == nil || !strings.Contains(serverErr.Error(), tc.refusal) || !strings.Contains(output.String(), serverErr.Error()) {
				t.Fatalf("refusal mismatch: server=%v CLI=%v: %s", serverErr, cliErr, output.String())
			}
		})
	}
}
