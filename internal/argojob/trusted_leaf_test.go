package argojob

import (
	"errors"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/internal/vmrunner"
)

func minimalDAGWorkflow() *wfv1.Workflow {
	return &wfv1.Workflow{
		Spec: wfv1.WorkflowSpec{
			Entrypoint: "main",
			Templates: []wfv1.Template{
				{
					Name: "main",
					DAG: &wfv1.DAGTemplate{
						Tasks: []wfv1.DAGTask{
							{Name: "build", Template: "build-step"},
						},
					},
				},
				{
					Name:      "build-step",
					Container: &corev1.Container{Image: "golang:1.26"},
				},
			},
		},
	}
}

func trustedConfig() Config {
	return Config{
		TrustedTestsEnabled:      true,
		TrustedTestsHarnessImage: "example.com/harness@sha256:" + strings.Repeat("a", 64),
		KVMEnabled:               true,
	}
}

func trustedRequest() Request {
	return Request{
		SHA: strings.Repeat("c", 40),
		TrustedTestContract: &vmrunner.Contract{
			Version:    2,
			Profile:    vmrunner.KVMSuiteProfile,
			Artifact:   vmrunner.BPFArtifact,
			GuestImage: "example.com/guest@sha256:" + strings.Repeat("b", 64),
			Resources:  vmrunner.ContractResources{CPUCores: 2, MemoryMiB: 4096},
			Timeout:    30 * time.Minute,
		},
		TrustedTestSuiteIdentity: strings.Repeat("d", 40),
	}
}

func TestAppendTrustedTestsLeafInjectsTemplate(t *testing.T) {
	t.Parallel()
	workflow := minimalDAGWorkflow()
	config := trustedConfig()
	request := trustedRequest()

	if err := appendTrustedTestsLeaf(workflow, config, request); err != nil {
		t.Fatal(err)
	}

	// Should have added one template.
	found := false
	for _, tmpl := range workflow.Spec.Templates {
		if tmpl.Name == vmrunner.BurnName {
			found = true
			if tmpl.Container == nil {
				t.Fatal("trusted leaf has no container")
			}
			if tmpl.Container.Image != config.TrustedTestsHarnessImage {
				t.Fatalf("image = %q, want %q", tmpl.Container.Image, config.TrustedTestsHarnessImage)
			}
			// Check KVM device was injected.
			if _, ok := tmpl.Container.Resources.Requests["devices.kubevirt.io/kvm"]; !ok {
				t.Fatal("KVM device resource not injected")
			}
			// Check automount is false.
			if tmpl.AutomountServiceAccountToken == nil || *tmpl.AutomountServiceAccountToken {
				t.Fatal("automountServiceAccountToken must be false")
			}
			break
		}
	}
	if !found {
		t.Fatal("trusted-tests template not found in workflow")
	}

	// Should have wired into the DAG.
	entrypoint := findEntrypointTemplate(workflow)
	if entrypoint == nil || entrypoint.DAG == nil {
		t.Fatal("no DAG in entrypoint")
	}
	dagTask := entrypoint.DAG.Tasks[len(entrypoint.DAG.Tasks)-1]
	if dagTask.Name != vmrunner.BurnName {
		t.Fatalf("last DAG task = %q, want %q", dagTask.Name, vmrunner.BurnName)
	}
	if len(dagTask.Dependencies) != 1 || dagTask.Dependencies[0] != "build" {
		t.Fatalf("dependencies = %v, want [build]", dagTask.Dependencies)
	}
}

func TestAppendTrustedTestsLeafNoopWithoutContract(t *testing.T) {
	t.Parallel()
	workflow := minimalDAGWorkflow()
	config := trustedConfig()
	request := Request{SHA: strings.Repeat("c", 40)} // no contract

	if err := appendTrustedTestsLeaf(workflow, config, request); err != nil {
		t.Fatal(err)
	}
	// Template count should be unchanged.
	if len(workflow.Spec.Templates) != 2 {
		t.Fatalf("templates = %d, want 2 (unchanged)", len(workflow.Spec.Templates))
	}
}

func TestAppendTrustedTestsLeafRefusesWhenDisabled(t *testing.T) {
	t.Parallel()
	workflow := minimalDAGWorkflow()
	config := trustedConfig()
	config.TrustedTestsEnabled = false
	request := trustedRequest()

	err := appendTrustedTestsLeaf(workflow, config, request)
	if !errors.Is(err, ErrTrustedTestsNotEnabled) {
		t.Fatalf("error = %v, want ErrTrustedTestsNotEnabled", err)
	}
}

func TestAppendTrustedTestsLeafRefusesNameCollision(t *testing.T) {
	t.Parallel()
	workflow := minimalDAGWorkflow()
	// Add a template with the reserved name.
	workflow.Spec.Templates = append(workflow.Spec.Templates, wfv1.Template{
		Name:      vmrunner.BurnName,
		Container: &corev1.Container{Image: "fake"},
	})
	config := trustedConfig()
	request := trustedRequest()

	err := appendTrustedTestsLeaf(workflow, config, request)
	if err == nil || !strings.Contains(err.Error(), "server-reserved") {
		t.Fatalf("error = %v, want server-reserved collision error", err)
	}
}

func TestAppendTrustedTestsLeafNoKVMWhenDisabled(t *testing.T) {
	t.Parallel()
	workflow := minimalDAGWorkflow()
	config := trustedConfig()
	config.KVMEnabled = false
	request := trustedRequest()

	if err := appendTrustedTestsLeaf(workflow, config, request); err != nil {
		t.Fatal(err)
	}
	for _, tmpl := range workflow.Spec.Templates {
		if tmpl.Name == vmrunner.BurnName {
			if _, ok := tmpl.Container.Resources.Requests["devices.kubevirt.io/kvm"]; ok {
				t.Fatal("KVM device injected when KVM is disabled")
			}
			return
		}
	}
	t.Fatal("trusted-tests template not found")
}
