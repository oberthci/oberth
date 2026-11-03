package argoworkflow

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

func hostVMWorkflow(t *testing.T) *wfv1.Workflow {
	t.Helper()
	raw, err := yaml.YAMLToJSONStrict([]byte(admittedVMIManifest))
	if err != nil {
		t.Fatal(err)
	}
	var manifest vmiManifest
	if err = json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Spec.Domain.Resources.Requests.CPU = "1500m"
	manifest.Spec.Domain.Resources.Limits.Memory = "4Gi"
	for i := range manifest.Spec.Volumes {
		if disk := manifest.Spec.Volumes[i].ContainerDisk; disk != nil {
			disk.Image = "example.invalid/repo-01@sha256:" + strings.Repeat("a", 64)
		}
	}
	raw, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "step-abc12345", Templates: []wfv1.Template{
		{Name: "step-abc12345", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "step-11111111", Template: "step-11111111"}}}, {Steps: []wfv1.WorkflowStep{{Name: "step-22222222", Template: "step-22222222"}}}}},
		{Name: "step-11111111", Resource: &wfv1.ResourceTemplate{Action: "create", SetOwnerReference: true, Manifest: string(raw)}}, declaredLeaf("step-22222222", "1", "1Gi"),
	}}}
	return wf
}

func TestHostVMLifetimePeakAndCellMemoryLimits(t *testing.T) {
	wf := hostVMWorkflow(t)
	var a vmiManifest
	if err := json.Unmarshal([]byte(wf.Spec.Templates[1].Resource.Manifest), &a); err != nil {
		t.Fatal(err)
	}
	if err := admitVMIResource("step-11111111", wf.Spec.Templates[1].Resource, []string{"example.invalid/"}); err != nil {
		t.Fatal(err)
	}
	cell, err := vmiCreateUsage(wf.Spec.Templates[1].Resource.Manifest)
	if err != nil || cell.cores != 2 || cell.memory.Value() != 4<<30 {
		t.Fatal("cell quota ignored declared memory limit")
	}
	b := a
	b.Metadata.Name = "{{workflow.name}}-step-33333333"
	b.Spec.Domain.Resources.Requests.CPU = "500m"
	b.Spec.Domain.Resources.Requests.Memory = "1Gi"
	b.Spec.Domain.Resources.Limits.Memory = "2Gi"
	braw, _ := json.Marshal(b)
	var deletion vmiManifest
	deletion.APIVersion = a.APIVersion
	deletion.Kind = a.Kind
	deletion.Metadata.Name = a.Metadata.Name
	draw, _ := json.Marshal(deletion)
	wf.Spec.Templates[0].Steps = []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "step-11111111", Template: "step-11111111"}}}, {Steps: []wfv1.WorkflowStep{{Name: "step-22222222", Template: "step-22222222"}}}, {Steps: []wfv1.WorkflowStep{{Name: "step-33333333", Template: "step-33333333"}}}}
	wf.Spec.Templates[2] = wfv1.Template{Name: "step-22222222", Resource: &wfv1.ResourceTemplate{Action: "delete", Manifest: string(draw)}}
	wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: "step-33333333", Resource: &wfv1.ResourceTemplate{Action: "create", Manifest: string(braw)}})
	d, err := DeclaredHostPeak(wf, RunTriggerBranch)
	if err != nil || d == nil || d.CPUMilli != 1500+VMILauncherCPUMilli || d.MemoryBytes != (4<<30)+VMILauncherMemoryBytes {
		t.Fatalf("lifetime peak: %+v %v", d, err)
	}
}

func TestVMLimitsOnlyAndInvalidLimitCannotBypassCellPolicy(t *testing.T) {
	wf := hostVMWorkflow(t)
	r := wf.Spec.Templates[1].Resource
	var m vmiManifest
	_ = json.Unmarshal([]byte(r.Manifest), &m)
	m.Spec.Domain.Resources.Requests.CPU = ""
	m.Spec.Domain.Resources.Requests.Memory = ""
	m.Spec.Domain.Resources.Limits.CPU = "2"
	raw, _ := json.Marshal(m)
	r.Manifest = string(raw)
	if err := admitVMIResource("step-11111111", r, []string{"example.invalid/"}); err != nil {
		t.Fatal(err)
	}
	d, err := DeclaredHostPeak(wf, RunTriggerBranch)
	if err != nil || d == nil || d.CPUMilli != 3000+VMILauncherCPUMilli {
		t.Fatalf("limits-only fallback: %+v %v", d, err)
	}
	m.Spec.Domain.Resources.Limits.CPU = "3"
	raw, _ = json.Marshal(m)
	r.Manifest = string(raw)
	if err := admitVMIResource("step-11111111", r, []string{"example.invalid/"}); err == nil {
		t.Fatal("CPU limit exceeded cell cores")
	}
}

func TestUndeclaredVMKeepsLegacyAndConservativeCellQuota(t *testing.T) {
	wf := hostVMWorkflow(t)
	r := wf.Spec.Templates[1].Resource
	var m vmiManifest
	_ = json.Unmarshal([]byte(r.Manifest), &m)
	m.Spec.Domain.Resources.Requests.CPU = ""
	m.Spec.Domain.Resources.Requests.Memory = ""
	m.Spec.Domain.Resources.Limits.CPU = ""
	m.Spec.Domain.Resources.Limits.Memory = ""
	raw, _ := json.Marshal(m)
	r.Manifest = string(raw)
	if err := admitVMIResource("step-11111111", r, []string{"example.invalid/"}); err != nil {
		t.Fatal(err)
	}
	cell, err := vmiCreateUsage(r.Manifest)
	if err != nil || cell.cores != 2 || cell.memory.Value() != VMIInstanceLimit().MemoryBytes {
		t.Fatal("undeclared VM escaped cell accounting")
	}
	if demand, err := DeclaredHostPeak(wf, RunTriggerBranch); demand != nil || !errors.Is(err, ErrVMHostUndeclared) {
		t.Fatalf("undeclared VM got host credit: %+v %v", demand, err)
	}
}

func TestDeclaredHostPeakUsesVMRequestsLimitsAndLauncherReserve(t *testing.T) {
	wf := hostVMWorkflow(t)
	d, err := DeclaredHostPeak(wf, RunTriggerBranch)
	if err != nil || d == nil || d.CPUMilli != 2500+VMILauncherCPUMilli || d.MemoryBytes != (5<<30)+VMILauncherMemoryBytes {
		t.Fatalf("host demand: %+v %v", d, err)
	}
	// Missing VM scheduling resources must earn no numeric admission credit.
	wf.Spec.Templates[1].Resource.Manifest = `{"apiVersion":"` + vmiVersion() + `","kind":"VirtualMachineInstance","metadata":{"name":"step-11111111"},"spec":{"domain":{"cpu":{"cores":2}}}}`
	d, err = DeclaredHostPeak(wf, RunTriggerBranch)
	if d != nil {
		t.Fatalf("undeclared VM got numeric credit: %+v %v", d, err)
	}
}

// Read the version from the existing fixture rather than introduce another identity.
func vmiVersion() string {
	var m vmiManifest
	b, _ := yaml.YAMLToJSONStrict([]byte(admittedVMIManifest))
	_ = json.Unmarshal(b, &m)
	return m.APIVersion
}

func TestMemoryReservationUsesRequestWithDirectAndInheritedLimit(t *testing.T) {
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "step-abc12345", Templates: []wfv1.Template{declaredLeaf("step-abc12345", "1", "128Mi")}}}
	wf.Spec.Templates[0].Container.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}
	d, err := DeclaredHostPeak(wf, RunTriggerBranch)
	if err != nil || d == nil || d.MemoryBytes != 128<<20 {
		t.Fatalf("memory request not reserved: %+v %v", d, err)
	}
	wf.Spec.TemplateDefaults = &wfv1.Template{Container: wf.Spec.Templates[0].Container.DeepCopy()}
	wf.Spec.Templates[0].Container.Resources.Limits = nil
	d, err = DeclaredHostPeak(wf, RunTriggerBranch)
	if err != nil || d == nil || d.MemoryBytes != 128<<20 {
		t.Fatalf("memory request with inherited limit not reserved: %+v %v", d, err)
	}
}
