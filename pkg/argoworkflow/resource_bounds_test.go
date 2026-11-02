package argoworkflow

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/oberthci/oberth/pkg/pipeline"
)

func resourceLeaf(name string, cpu, memory string) wfv1.Template {
	requests := corev1.ResourceList{}
	if cpu != "" {
		requests[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if memory != "" {
		requests[corev1.ResourceMemory] = resource.MustParse(memory)
	}
	return wfv1.Template{Name: name, Container: &corev1.Container{Resources: corev1.ResourceRequirements{Requests: requests}}}
}

func TestResourceBoundsDeadlineNegativeAndRequestLimitValidation(t *testing.T) {
	for _, kind := range []string{"deadline", "negative", "request-limit", "grant"} {
		t.Run(kind, func(t *testing.T) {
			wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "step-abc12345", Templates: []wfv1.Template{resourceLeaf("step-abc12345", "1", "128Mi")}}}
			maximum := pipeline.L
			switch kind {
			case "deadline":
				value := intstr.FromInt(3601)
				wf.Spec.Templates[0].ActiveDeadlineSeconds = &value
			case "negative":
				wf.Spec.Templates[0].Container.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("-1")
			case "request-limit":
				wf.Spec.Templates[0].Container.Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}
			case "grant":
				maximum = pipeline.Size("invalid")
			}
			if err := ValidateResourceCeiling(wf, "repo-01", maximum, 3600); err == nil {
				t.Fatal("invalid declaration accepted")
			}
		})
	}
}
func TestResourceBoundsRespectServerGrantWithoutMutatingDeclarations(t *testing.T) {
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "step-abc12345", Templates: []wfv1.Template{resourceLeaf("step-abc12345", "5", "1Gi")}}}
	before := wf.DeepCopy()
	err := ValidateResourceCeiling(wf, "repo-01", pipeline.L, 3600)
	if err == nil || !strings.Contains(err.Error(), "repo-01") || !strings.Contains(err.Error(), "step-abc12345") || !strings.Contains(err.Error(), "requests.cpu 5") || !strings.Contains(err.Error(), "ceiling 4") {
		t.Fatalf("exact refusal: %v", err)
	}
	if err := ValidateResourceCeiling(wf, "repo-01", pipeline.XL, 3600); err != nil {
		t.Fatal(err)
	}
	if !wf.Spec.Templates[0].Container.Resources.Requests.Cpu().Equal(*before.Spec.Templates[0].Container.Resources.Requests.Cpu()) {
		t.Fatal("resources changed")
	}
	if wf.Spec.Templates[0].Container.Resources.Limits != nil {
		t.Fatal("CPU limit added")
	}
}
func TestResourceBoundsCoverDefaultsInlineSetsHelpersAndLimits(t *testing.T) {
	for _, shape := range []string{"default", "inline", "set", "sidecar", "init", "limit"} {
		t.Run(shape, func(t *testing.T) {
			wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "step-abc12345", Templates: []wfv1.Template{resourceLeaf("step-abc12345", "1", "1Gi")}}}
			big := resourceLeaf("step-12345678", "5", "1Gi")
			switch shape {
			case "default":
				wf.Spec.TemplateDefaults = &big
			case "inline":
				wf.Spec.Templates[0] = wfv1.Template{Name: "step-abc12345", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "step-12345678", Inline: &big}}}}
			case "set":
				wf.Spec.Templates[0].Container = nil
				wf.Spec.Templates[0].ContainerSet = &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: *big.Container}}}
			case "sidecar":
				wf.Spec.Templates[0].Sidecars = []wfv1.UserContainer{{Container: *big.Container}}
			case "init":
				wf.Spec.Templates[0].InitContainers = []wfv1.UserContainer{{Container: *big.Container}}
			case "limit":
				wf.Spec.Templates[0].Container.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("9Gi")}
			}
			if err := ValidateResourceCeiling(wf, "repo-01", pipeline.L, 3600); err == nil {
				t.Fatal("ceiling bypass")
			}
		})
	}
}
func TestResourceCoverageIncludesRootNestedRepeatedAndDynamicCalls(t *testing.T) {
	declared := resourceLeaf("step-abc12345", "100m", "128Mi")
	missing := resourceLeaf("step-12345678", "", "128Mi")
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: declared.Name, Templates: []wfv1.Template{declared, missing}}}
	if got := ResourceCoverage(wf); got == nil || got.Declared != 1 || got.Undeclared != 0 {
		t.Fatalf("root: %+v", got)
	}
	wf.Spec.Entrypoint = "step-00000000"
	wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: wf.Spec.Entrypoint, DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "step-11111111", Template: declared.Name}, {Name: "step-22222222", Template: declared.Name}, {Name: "step-33333333", Template: missing.Name, WithParam: "{{inputs.parameters.items}}"}}}})
	if got := ResourceCoverage(wf); got == nil || got.Declared != 2 || got.Undeclared != 1 {
		t.Fatalf("calls: %+v", got)
	}
}
