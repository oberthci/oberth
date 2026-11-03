package argoworkflow

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func memoryLimitedLeaf(name string) wfv1.Template {
	t := declaredLeaf(name, "100m", "16Mi")
	t.Container.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("32Mi")}
	return t
}

func TestDeclaredMemoryLimitsRequiredForEveryPodContainer(t *testing.T) {
	for _, shape := range []string{"request-only", "zero-limit", "init", "sidecar", "container-set", "inherited-init", "inherited-sidecar", "dynamic-sidecar", "all-limited", "inherited-main", "inherited-helper-limit", "replaced-container-set", "script-inherited-limit", "script-wrong-default-type"} {
		t.Run(shape, func(t *testing.T) {
			leaf := memoryLimitedLeaf("step-abc12345")
			helper := wfv1.UserContainer{Container: *memoryLimitedLeaf("step-11111111").Container}
			helper.Name = "step-11111111"
			var defaults *wfv1.Template
			wantNumeric := false
			switch shape {
			case "request-only":
				leaf.Container.Resources.Limits = nil
			case "zero-limit":
				leaf.Container.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("0")
			case "init", "sidecar", "inherited-init", "inherited-sidecar":
				helper.Resources.Limits = nil
				if strings.Contains(shape, "init") {
					leaf.InitContainers = []wfv1.UserContainer{helper}
				} else {
					leaf.Sidecars = []wfv1.UserContainer{helper}
				}
				if strings.HasPrefix(shape, "inherited-") {
					defaults = &wfv1.Template{InitContainers: leaf.InitContainers, Sidecars: leaf.Sidecars}
					leaf.InitContainers, leaf.Sidecars = nil, nil
				}
			case "container-set":
				leaf.Container = nil
				helper.Resources.Limits = nil
				leaf.ContainerSet = &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: helper.Container}}}
			case "replaced-container-set":
				leaf.Container = nil
				defaults = &wfv1.Template{ContainerSet: &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: helper.Container}}}}
				helper.Resources.Limits = nil
				leaf.ContainerSet = &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: helper.Container}}}
			case "script-inherited-limit", "script-wrong-default-type":
				if shape == "script-inherited-limit" {
					defaults = &wfv1.Template{Script: &wfv1.ScriptTemplate{Container: *leaf.Container.DeepCopy()}}
					wantNumeric = true
				} else {
					defaults = &wfv1.Template{Container: leaf.Container.DeepCopy()}
				}
				leaf.Container.Resources.Limits = nil
				leaf.Script = &wfv1.ScriptTemplate{Container: *leaf.Container}
				leaf.Container = nil
			case "dynamic-sidecar":
				helper.Resources.Limits = nil
				leaf.Sidecars = []wfv1.UserContainer{helper}
			case "all-limited":
				leaf.InitContainers, leaf.Sidecars = []wfv1.UserContainer{helper}, []wfv1.UserContainer{helper}
				wantNumeric = true
			case "inherited-main":
				defaults = &wfv1.Template{Container: leaf.Container.DeepCopy()}
				leaf.Container.Resources.Limits = nil
				wantNumeric = true
			case "inherited-helper-limit":
				defaults = &wfv1.Template{Sidecars: []wfv1.UserContainer{helper}}
				helper.Resources.Limits = nil
				leaf.Sidecars = []wfv1.UserContainer{helper}
				wantNumeric = true
			}
			wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: leaf.Name, Templates: []wfv1.Template{leaf}, TemplateDefaults: defaults}}
			if shape == "dynamic-sidecar" {
				p := int64(2)
				wf.Spec.Parallelism = &p
				wf.Spec.Entrypoint = "step-22222222"
				wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: wf.Spec.Entrypoint, Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: leaf.Name, Template: leaf.Name, WithParam: "{{inputs.parameters.items}}"}}}}})
			}
			before := wf.DeepCopy()
			demand, err := DeclaredHostPeak(wf, RunTriggerBranch)
			if wantNumeric {
				if err != nil || demand == nil {
					t.Fatalf("bounded pod lost numeric eligibility: %+v %v", demand, err)
				}
				if shape == "inherited-helper-limit" && demand.MemoryBytes != 32<<20 {
					t.Fatalf("inherited helper counted incorrectly: %+v", demand)
				}
			} else if demand != nil || err == nil || !strings.Contains(err.Error(), "memory limit") {
				t.Fatalf("missing memory limit gained numeric eligibility: %+v %v", demand, err)
			}
			if !reflect.DeepEqual(wf, before) {
				t.Fatal("declarations changed")
			}
		})
	}
}

func TestVMIRequestMemoryBoundRejectsGuestOverride(t *testing.T) {
	wf := hostVMWorkflow(t)
	r := wf.Spec.Templates[1].Resource
	var manifest map[string]any
	if err := json.Unmarshal([]byte(r.Manifest), &manifest); err != nil {
		t.Fatal(err)
	}
	domain := manifest["spec"].(map[string]any)["domain"].(map[string]any)
	domain["memory"] = map[string]any{"guest": "64Gi"}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	r.Manifest = string(raw)
	if err := admitVMIResource("step-11111111", r, []string{"example.invalid/"}); err == nil || !strings.Contains(err.Error(), "unapproved field") {
		t.Fatalf("guest memory override admitted: %v", err)
	}
}

func TestDeclaredVMGuestMemoryBoundAndLauncherContainerLimits(t *testing.T) {
	for _, shape := range []string{"vmi-request-only", "vmi-memory-undeclared", "launcher-init", "launcher-sidecar", "ordinary-step", "all-limited", "launcher-helpers-limited"} {
		t.Run(shape, func(t *testing.T) {
			wf := hostVMWorkflow(t)
			wf.Spec.Templates[2].Container.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
			helper := wfv1.UserContainer{Container: *memoryLimitedLeaf("step-44444444").Container}
			helper.Resources.Limits = nil
			switch shape {
			case "vmi-request-only", "vmi-memory-undeclared":
				var m vmiManifest
				if err := json.Unmarshal([]byte(wf.Spec.Templates[1].Resource.Manifest), &m); err != nil {
					t.Fatal(err)
				}
				m.Spec.Domain.Resources.Limits.Memory = ""
				if shape == "vmi-memory-undeclared" {
					m.Spec.Domain.Resources.Requests.Memory = ""
				}
				raw, err := json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				wf.Spec.Templates[1].Resource.Manifest = string(raw)
			case "launcher-init":
				wf.Spec.Templates[1].InitContainers = []wfv1.UserContainer{helper}
			case "launcher-sidecar":
				wf.Spec.Templates[1].Sidecars = []wfv1.UserContainer{helper}
			case "ordinary-step":
				wf.Spec.Templates[2].Container.Resources.Limits = nil
			case "launcher-helpers-limited":
				helper.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("32Mi")}
				wf.Spec.Templates[1].InitContainers = []wfv1.UserContainer{helper}
				wf.Spec.Templates[1].Sidecars = []wfv1.UserContainer{helper}
			}
			before := wf.DeepCopy()
			if shape == "vmi-request-only" {
				if err := admitVMIResource("step-11111111", wf.Spec.Templates[1].Resource, []string{"example.invalid/"}); err != nil {
					t.Fatalf("request-only VMI refused by policy: %v", err)
				}
			}
			demand, err := DeclaredHostPeak(wf, RunTriggerBranch)
			if shape == "all-limited" || shape == "launcher-helpers-limited" || shape == "vmi-request-only" {
				if err != nil || demand == nil {
					t.Fatalf("bounded VM lost numeric eligibility: %+v %v", demand, err)
				}
				if shape == "vmi-request-only" && demand.MemoryBytes != (3<<30)+VMILauncherMemoryBytes {
					t.Fatalf("request-only VMI did not reserve guest memory plus launcher: %+v", demand)
				}
			} else if shape == "vmi-memory-undeclared" {
				if demand != nil || !errors.Is(err, ErrVMHostUndeclared) {
					t.Fatalf("VMI without memory declaration gained numeric credit: %+v %v", demand, err)
				}
			} else if demand != nil || err == nil || !strings.Contains(err.Error(), "memory limit") {
				t.Fatalf("unbounded VM pod gained numeric eligibility: %+v %v", demand, err)
			}
			if !reflect.DeepEqual(wf, before) {
				t.Fatal("VM manifest changed")
			}
		})
	}
}

func TestPodRequestChecksPrecedeMissingMemoryLimit(t *testing.T) {
	for _, missing := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		t.Run(string(missing), func(t *testing.T) {
			c := declaredLeaf("step-abc12345", "100m", "16Mi").Container
			c.Resources.Limits = nil
			delete(c.Resources.Requests, missing)
			if _, err := containerDemand(c, nil); !errors.Is(err, errPodUndeclared) {
				t.Fatalf("missing request reported as missing limit: %v", err)
			}
		})
	}
}
