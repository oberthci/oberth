package argoworkflow

import (
	"fmt"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func declaredLeaf(name, cpu, memory string) wfv1.Template {
	return wfv1.Template{Name: name, Container: &corev1.Container{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(memory)}}}}
}

func TestDeclaredPodPeakMatchesExhaustiveSmallDAGs(t *testing.T) {
	// Enumerate every edge subset of a four-node acyclic graph, then compare
	// against an independent brute-force concurrent-subset oracle.
	for mask := 0; mask < 64; mask++ {
		wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "step-abc12345"}}
		tasks := make([]wfv1.DAGTask, 4)
		reach := [4][4]bool{}
		bit := 0
		for i := 0; i < 4; i++ {
			name := fmt.Sprintf("step-%08x", i+1)
			tasks[i] = wfv1.DAGTask{Name: name, Template: name}
			wf.Spec.Templates = append(wf.Spec.Templates, declaredLeaf(name, fmt.Sprint(i+1), fmt.Sprintf("%dMi", 4-i)))
			for j := 0; j < i; j++ {
				if mask&(1<<bit) != 0 {
					reach[j][i] = true
					tasks[i].Dependencies = append(tasks[i].Dependencies, fmt.Sprintf("step-%08x", j+1))
				}
				bit++
			}
		}
		wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: "step-abc12345", DAG: &wfv1.DAGTemplate{Tasks: tasks}})
		for k := 0; k < 4; k++ {
			for i := 0; i < 4; i++ {
				for j := 0; j < 4; j++ {
					reach[i][j] = reach[i][j] || (reach[i][k] && reach[k][j])
				}
			}
		}
		var cpu, mem, pods int64
		for subset := 1; subset < 16; subset++ {
			valid := true
			var c, m, p int64
			for i := 0; i < 4; i++ {
				if subset&(1<<i) == 0 {
					continue
				}
				c += int64(i+1) * 1000
				m += int64(4-i) << 20
				p++
				for j := i + 1; j < 4; j++ {
					if subset&(1<<j) != 0 && reach[i][j] {
						valid = false
					}
				}
			}
			if valid {
				cpu = max(cpu, c)
				mem = max(mem, m)
				pods = max(pods, p)
			}
		}
		d, err := DeclaredPodPeak(wf)
		if err != nil || d == nil || d.CPUMilli != cpu || d.MemoryBytes != mem || d.Pods != pods {
			t.Fatalf("DAG %d: got %+v %v; want %d/%d/%d", mask, d, err, cpu, mem, pods)
		}
	}
}

func TestDeclaredPodPeakStaticFanoutAndParallelism(t *testing.T) {
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "step-abc12345", Templates: []wfv1.Template{
		{Name: "step-abc12345", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "step-11111111", Template: "step-11111111", WithParam: "[1,2,3]"}}}}, declaredLeaf("step-11111111", "1", "1Gi"),
	}}}
	d, err := DeclaredPodPeak(wf)
	if err != nil || d == nil || d.Pods != 3 || d.CPUMilli != 3000 {
		t.Fatalf("static fanout: %+v %v", d, err)
	}
	p := int64(2)
	wf.Spec.Parallelism = &p
	d, err = DeclaredPodPeak(wf)
	if err != nil || d == nil || d.Pods != 2 || d.CPUMilli != 2000 {
		t.Fatalf("parallelism: %+v %v", d, err)
	}
}

func TestDeclaredPodPeakUsesDependencyConcurrency(t *testing.T) {
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "step-abc12345", Templates: []wfv1.Template{
		{Name: "step-abc12345", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
			{Name: "step-11111111", Template: "step-11111111"},
			{Name: "step-22222222", Template: "step-22222222"},
			{Name: "step-33333333", Template: "step-33333333", Dependencies: []string{"step-11111111", "step-22222222"}},
		}}}, declaredLeaf("step-11111111", "2", "1Gi"), declaredLeaf("step-22222222", "1", "2Gi"), declaredLeaf("step-33333333", "1", "1Gi"),
	}}}
	d, err := DeclaredPodPeak(wf)
	if err != nil || d == nil || d.CPUMilli != 3000 || d.MemoryBytes != 3<<30 || d.Pods != 2 {
		t.Fatalf("peak: %+v %v", d, err)
	}
	// This dependency has an early-start alternative: it earns no ordering credit.
	wf.Spec.Templates[0].DAG.Tasks[2].Dependencies = nil
	wf.Spec.Templates[0].DAG.Tasks[2].Depends = "step-11111111.Running || step-22222222.Succeeded"
	d, err = DeclaredPodPeak(wf)
	if err != nil || d == nil || d.CPUMilli != 4000 || d.Pods != 3 {
		t.Fatalf("conditional dependency undercount: %+v %v", d, err)
	}
}

func TestDeclaredPodPeakDefaultsHelpersAndUnknowns(t *testing.T) {
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "step-abc12345", Templates: []wfv1.Template{declaredLeaf("step-abc12345", "1", "1Gi")}}}
	wf.Spec.Templates[0].Sidecars = []wfv1.UserContainer{{Container: *declaredLeaf("main", "100m", "128Mi").Container}}
	wf.Spec.Templates[0].InitContainers = []wfv1.UserContainer{{Container: *declaredLeaf("main", "2", "256Mi").Container}}
	d, err := DeclaredPodPeak(wf)
	if err != nil || d == nil || d.CPUMilli != 2000 || d.MemoryBytes != (1<<30)+(128<<20) {
		t.Fatalf("helper scheduling requests: %+v %v", d, err)
	}
	wf.Spec.Templates[0].Container.Resources.Requests = nil
	d, err = DeclaredPodPeak(wf)
	if err != nil || d != nil {
		t.Fatalf("undeclared must retain size class: %+v %v", d, err)
	}
	wf.Spec.TemplateDefaults = &wfv1.Template{Container: declaredLeaf("main", "1", "1Gi").Container}
	d, err = DeclaredPodPeak(wf)
	if err != nil || d == nil {
		t.Fatalf("defaults not recognized: %+v %v", d, err)
	}
}

func TestDeclaredPodPeakDynamicFanoutRequiresBound(t *testing.T) {
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "step-abc12345", Templates: []wfv1.Template{
		{Name: "step-abc12345", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "step-11111111", Template: "step-11111111", WithParam: "{{inputs.parameters.items}}"}}}}}, declaredLeaf("step-11111111", "1", "1Gi"),
	}}}
	if _, err := DeclaredPodPeak(wf); err == nil {
		t.Fatal("unbounded fanout admitted")
	}
	// Unused utilities must not turn a fully declared reachable graph into legacy admission.
	wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: "step-99999999", Container: &corev1.Container{}})
	limit := int64(3)
	wf.Spec.Parallelism = &limit
	d, err := DeclaredPodPeak(wf)
	if err != nil || d == nil || d.CPUMilli != 3000 || d.MemoryBytes != 3<<30 || d.Pods != 3 {
		t.Fatalf("bounded fanout: %+v %v", d, err)
	}
}
