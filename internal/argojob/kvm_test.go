package argojob

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

const kvmTestDocument = `apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/kvm-templates: kernel-test
spec:
  entrypoint: pipeline
  activeDeadlineSeconds: 600
  templates:
  - name: pipeline
    dag:
      tasks:
      - name: build
        template: build
      - name: test
        template: kernel-test
        depends: build
  - name: build
    container:
      image: golang:1.26.6-trixie@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      command: [/bin/true]
  - name: kernel-test
    metadata:
      annotations:
        oberth.ci/workspace-mounts: "none"
        oberth.ci/workspace-env: "none"
    automountServiceAccountToken: false
    inputs:
      parameters:
      - name: kernel-version
        default: "6.12"
      - name: arch
        default: amd64
      - name: suite
        default: bpf
      - name: qemu-image
        default: "kernel-6.12@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
      - name: timeout
        default: "300"
    container:
      image: golang:1.26.6-trixie@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      command: [/bin/sh, -c]
      args: ["echo running {{inputs.parameters.kernel-version}} {{inputs.parameters.arch}}"]
      resources:
        requests:
          cpu: "2"
          memory: 6Gi
        limits:
          cpu: "2"
          memory: 6Gi
`

// TestBuildKVMLeaves proves that when KVMEnabled is true, declared KVM leaves
// get devices.kubevirt.io/kvm: "1" in both requests and limits, and
// OBERTH_KVM=1 in their env. Sibling leaves must be untouched.
func TestBuildKVMLeaves(t *testing.T) {
	config := testConfig()
	config.KVMEnabled = true
	request := testRequest(periapsis.TriggerCI, kvmTestDocument)
	request.SourceVolume = SourceVolume{ClaimName: "run-source", SubPath: "src", ArtifactsSubPath: "artifacts"}

	wf, err := Build(config, request)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Find kernel-test template and verify KVM device injection.
	var kvmTmpl, buildTmpl *wfv1.Template
	for i := range wf.Spec.Templates {
		switch wf.Spec.Templates[i].Name {
		case "kernel-test":
			kvmTmpl = &wf.Spec.Templates[i]
		case "build":
			buildTmpl = &wf.Spec.Templates[i]
		}
	}

	if kvmTmpl == nil {
		t.Fatal("kernel-test template not found in built workflow")
	}

	main := templateMainContainer(kvmTmpl)
	if main == nil {
		t.Fatal("kernel-test has no main container")
	}

	// Check requests.
	kvmReq, ok := main.Resources.Requests[corev1.ResourceName("devices.kubevirt.io/kvm")]
	if !ok {
		t.Fatal("kernel-test missing devices.kubevirt.io/kvm in requests")
	}
	if kvmReq.String() != "1" {
		t.Fatalf("kernel-test KVM request = %s, want 1", kvmReq.String())
	}

	// Check limits.
	kvmLim, ok := main.Resources.Limits[corev1.ResourceName("devices.kubevirt.io/kvm")]
	if !ok {
		t.Fatal("kernel-test missing devices.kubevirt.io/kvm in limits")
	}
	if kvmLim.String() != "1" {
		t.Fatalf("kernel-test KVM limit = %s, want 1", kvmLim.String())
	}

	// Check OBERTH_KVM=1 env.
	foundKVMEnv := false
	for _, env := range main.Env {
		if env.Name == "OBERTH_KVM" {
			if env.Value != "1" {
				t.Fatalf("OBERTH_KVM = %q, want 1", env.Value)
			}
			foundKVMEnv = true
		}
	}
	if !foundKVMEnv {
		t.Fatal("kernel-test missing OBERTH_KVM env var")
	}

	// Verify existing resources are preserved.
	cpuReq, ok := main.Resources.Requests[corev1.ResourceCPU]
	if !ok || cpuReq.String() != "2" {
		t.Fatalf("kernel-test CPU request = %v (ok=%v), want 2", cpuReq.String(), ok)
	}
	memReq, ok := main.Resources.Requests[corev1.ResourceMemory]
	if !ok || memReq.String() != "6Gi" {
		t.Fatalf("kernel-test memory request = %v (ok=%v), want 6Gi", memReq.String(), ok)
	}

	// Verify sibling build leaf is untouched -- no KVM device.
	if buildTmpl != nil {
		buildMain := templateMainContainer(buildTmpl)
		if buildMain != nil {
			if _, hasKVM := buildMain.Resources.Requests[corev1.ResourceName("devices.kubevirt.io/kvm")]; hasKVM {
				t.Fatal("sibling build template should not have KVM device")
			}
			for _, env := range buildMain.Env {
				if env.Name == "OBERTH_KVM" {
					t.Fatal("sibling build template should not have OBERTH_KVM")
				}
			}
		}
	}
}

// TestBuildKVMSwitchOff proves that declaring KVM leaves when KVMEnabled is
// false produces an infrastructure-class error, not silent TCG fallback.
func TestBuildKVMSwitchOff(t *testing.T) {
	config := testConfig()
	config.KVMEnabled = false
	request := testRequest(periapsis.TriggerCI, kvmTestDocument)
	request.SourceVolume = SourceVolume{ClaimName: "run-source", SubPath: "src", ArtifactsSubPath: "artifacts"}

	_, err := Build(config, request)
	if err == nil {
		t.Fatal("expected infrastructure error when KVMEnabled is false")
	}
	if !strings.Contains(err.Error(), "vm.kvm.enabled is false") {
		t.Fatalf("error should mention vm.kvm.enabled: %v", err)
	}
	if !strings.Contains(err.Error(), argoworkflow.KVMTemplatesAnnotation) {
		t.Fatalf("error should mention the annotation: %v", err)
	}
}

// TestBuildKVMNotDeclared proves that a workflow without the KVM annotation
// builds successfully regardless of KVMEnabled.
func TestBuildKVMNotDeclared(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		config := testConfig()
		config.KVMEnabled = enabled

		// Use the greedy document which has no KVM annotation.
		request := testRequest(periapsis.TriggerCI, greedyDocument)
		_, err := Build(config, request)
		if err != nil {
			t.Fatalf("KVMEnabled=%v: %v", enabled, err)
		}
	}
}
