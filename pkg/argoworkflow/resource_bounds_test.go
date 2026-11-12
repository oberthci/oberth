package argoworkflow

import (
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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
