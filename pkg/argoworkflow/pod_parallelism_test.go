package argoworkflow

import (
	"fmt"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func podLimit(n int64) *int64 { return &n }

func parallelFanout(kind, loop string, count int) wfv1.Template {
	t := wfv1.Template{Name: "root"}
	s := wfv1.WorkflowStep{Name: "workers", Template: "leaf"}
	switch loop {
	case "items":
		s.WithItems = make([]wfv1.Item, count)
	case "param":
		s.WithParam = "[0"
		for i := 1; i < count; i++ {
			s.WithParam += ",0"
		}
		s.WithParam += "]"
	case "sequence":
		n := intstr.FromInt(count)
		s.WithSequence = &wfv1.Sequence{Count: &n}
	case "dynamic":
		s.WithParam = "{{inputs.parameters.items}}"
	case "dynamic-sequence":
		n := intstr.FromString("{{inputs.parameters.count}}")
		s.WithSequence = &wfv1.Sequence{Count: &n}
	}
	if kind == "dag" {
		t.DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: s.Name, Template: s.Template, WithItems: s.WithItems, WithParam: s.WithParam, WithSequence: s.WithSequence}}}
	} else {
		t.Steps = []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{s}}}
	}
	return t
}

func assertPodPeak(t *testing.T, wf *wfv1.Workflow, cpu, memory, pods int64) {
	t.Helper()
	d, err := DeclaredPodPeak(wf)
	if err != nil || d == nil || d.CPUMilli != cpu || d.MemoryBytes != memory || d.Pods != pods {
		t.Fatalf("peak = %+v, %v; want CPU=%d memory=%d pods=%d", d, err, cpu, memory, pods)
	}
}

func TestPodParallelismFanouts(t *testing.T) {
	for _, kind := range []string{"steps", "dag"} {
		for _, loop := range []string{"items", "param", "sequence", "dynamic", "dynamic-sequence"} {
			for _, scope := range []string{"template", "workflow", "both", "defaults"} {
				t.Run(kind+"/"+loop+"/"+scope, func(t *testing.T) {
					root := parallelFanout(kind, loop, 18)
					wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "root", Templates: []wfv1.Template{root, declaredLeaf("leaf", "2", "6Gi")}}}
					switch scope {
					case "template":
						wf.Spec.Templates[0].Parallelism = podLimit(4)
					case "workflow":
						wf.Spec.Parallelism = podLimit(4)
					case "both":
						wf.Spec.Parallelism = podLimit(4)
						wf.Spec.Templates[0].Parallelism = podLimit(7)
					case "defaults":
						wf.Spec.TemplateDefaults = &wfv1.Template{Parallelism: podLimit(4)}
					}
					assertPodPeak(t, wf, 8000, 24<<30, 4)
				})
			}
		}
	}
}

func TestPodParallelismScopeAndMultiplicity(t *testing.T) {
	for _, kind := range []string{"steps", "dag"} {
		t.Run(kind, func(t *testing.T) {
			root := parallelFanout(kind, "param", 3)
			inner := parallelFanout(kind, "param", 18)
			inner.Name, inner.Parallelism = "leaf", podLimit(4)
			if kind == "dag" {
				inner.DAG.Tasks[0].Template = "worker"
			} else {
				inner.Steps[0].Steps[0].Template = "worker"
			}
			root.Parallelism = podLimit(2)
			wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "root", Templates: []wfv1.Template{root, inner, declaredLeaf("worker", "1", "1Gi")}}}
			// The outer limit caps child invocations, each of which can run four pods.
			assertPodPeak(t, wf, 8000, 8<<30, 8)
			wf.Spec.Parallelism = podLimit(3)
			assertPodPeak(t, wf, 3000, 3<<30, 3)
			wf.Spec.Parallelism = nil
			wf.Spec.Templates[0].Parallelism = nil
			assertPodPeak(t, wf, 12000, 12<<30, 12)
			wf.Spec.Templates[1].Parallelism = nil
			wf.Spec.Templates[2].Parallelism = podLimit(1)
			// A limit on a leaf never serializes separate calls to that leaf.
			assertPodPeak(t, wf, 54000, 54<<30, 54)
		})
	}
}

func TestPodParallelismSharedBoundaryAndIndependentDimensions(t *testing.T) {
	root := wfv1.Template{Name: "root", Parallelism: podLimit(2), DAG: &wfv1.DAGTemplate{}}
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "root"}}
	for i, cpu := range []int{7, 5, 1} {
		name := fmt.Sprintf("leaf-%d", i)
		root.DAG.Tasks = append(root.DAG.Tasks, wfv1.DAGTask{Name: name, Template: name, WithParam: "[1,2,3]"})
		wf.Spec.Templates = append(wf.Spec.Templates, declaredLeaf(name, fmt.Sprint(cpu), fmt.Sprintf("%dGi", 2*i+1)))
	}
	wf.Spec.Templates = append(wf.Spec.Templates, root)
	// The limit is shared across sibling loops; CPU and memory peaks differ.
	assertPodPeak(t, wf, 14000, 10<<30, 2)
}

func TestPodParallelismSmallLargeAndSequentialFanouts(t *testing.T) {
	for _, count := range []int{2, 18, 1000} {
		root := parallelFanout("steps", "items", count)
		root.Parallelism = podLimit(4)
		root.Steps = append(root.Steps, wfv1.ParallelSteps{Steps: []wfv1.WorkflowStep{{Name: "after", Template: "large"}}})
		wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "root", Templates: []wfv1.Template{root, declaredLeaf("leaf", "1", "1Gi"), declaredLeaf("large", "6", "2Gi")}}}
		assertPodPeak(t, wf, 6000, int64(min(count, 4))<<30, int64(min(count, 4)))
	}
}

func TestPodParallelismUnboundedInnerAndMissingDeclarations(t *testing.T) {
	root := parallelFanout("dag", "param", 3)
	root.Parallelism = podLimit(2)
	inner := parallelFanout("steps", "dynamic", 0)
	inner.Name = "leaf"
	inner.Steps[0].Steps[0].Template = "worker"
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "root", Templates: []wfv1.Template{root, inner, declaredLeaf("worker", "1", "1Gi")}}}
	if d, err := DeclaredPodPeak(wf); d != nil || err == nil {
		t.Fatalf("unbounded inner fanout got credit: %+v %v", d, err)
	}
	wf.Spec.Templates[1].Parallelism = podLimit(4)
	wf.Spec.Templates[2].Container.Resources.Limits = nil
	if d, err := DeclaredPodPeak(wf); d != nil || err != ErrMemoryUnbounded {
		t.Fatalf("missing limit got credit: %+v %v", d, err)
	}
}
