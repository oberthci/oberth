package argoworkflow

import (
	"fmt"
	"os"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func phaseWorkflow(tasks []wfv1.DAGTask) *wfv1.Workflow {
	wf := p2pVMIWorkflow()
	wf.Spec.Entrypoint = "main"
	for i := range wf.Spec.Templates {
		t := &wf.Spec.Templates[i]
		t.Resource.Manifest = strings.ReplaceAll(t.Resource.Manifest, "cores: 4", "cores: 2")
		t.Resource.Manifest = strings.ReplaceAll(t.Resource.Manifest, `cpu: "4"`, `cpu: "2"`)
		t.Resource.Manifest = strings.ReplaceAll(t.Resource.Manifest, "memory: 8Gi", "memory: 6Gi")
	}
	wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: tasks}})
	return wf
}

func vmTask(name, template string, deps ...string) wfv1.DAGTask {
	return wfv1.DAGTask{Name: name, Template: template, Dependencies: deps}
}

func TestVMILifetimePeak(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tasks []wfv1.DAGTask
		want  int
	}{
		{"sequential", []wfv1.DAGTask{vmTask("a", "create-0"), vmTask("da", "delete-0", "a"), vmTask("b", "create-1", "da"), vmTask("db", "delete-1", "b"), vmTask("c", "create-2", "db")}, 1},
		{"parallel", []wfv1.DAGTask{vmTask("a", "create-0"), vmTask("b", "create-1"), vmTask("c", "create-2")}, 3},
		{"diamond", []wfv1.DAGTask{vmTask("a", "create-0"), vmTask("b", "create-1"), vmTask("da", "delete-0", "a"), vmTask("db", "delete-1", "b"), vmTask("c", "create-2", "da", "db")}, 2},
		{"create-completion-is-not-deletion", []wfv1.DAGTask{vmTask("a", "create-0"), vmTask("b", "create-1", "a"), vmTask("c", "create-2", "b")}, 3},
		{"delete-not-ancestor", []wfv1.DAGTask{vmTask("a", "create-0"), vmTask("da", "delete-0", "a"), vmTask("b", "create-1", "a"), vmTask("c", "create-2", "b")}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wf := phaseWorkflow(tc.tasks)
			sum, err := SummedVMIDemand(wf)
			if err != nil || sum.VMs != 3 {
				t.Fatal(sum, err)
			}
			peak, err := DeclaredVMIDemand(wf)
			want := VMIDemand{VMs: tc.want, Cores: tc.want * 2, MemoryBytes: int64(tc.want) * 6 << 30}
			if err != nil || peak != want {
				t.Fatalf("sum=%+v peak=%+v want=%+v err=%v", sum, peak, want, err)
			}
		})
	}
}

func TestVMIPeakReusedTemplateAndExitCleanup(t *testing.T) {
	wf := phaseWorkflow([]wfv1.DAGTask{vmTask("a", "create-0"), vmTask("da", "delete-0", "a"), vmTask("a2", "create-0", "da"), vmTask("b", "create-1", "a2"), vmTask("c", "create-2", "b")})
	peak, err := DeclaredVMIDemand(wf)
	if err != nil || peak.VMs != 3 {
		t.Fatal(peak, err)
	}
	wf.Spec.Templates[len(wf.Spec.Templates)-1].DAG.Tasks[2].Dependencies = []string{"a"}
	peak, err = DeclaredVMIDemand(wf)
	if err != nil || peak.VMs != 4 {
		t.Fatalf("reuse without delete must count twice: %+v %v", peak, err)
	}
	wf.Spec.OnExit = "delete-0"
	peak, err = DeclaredVMIDemand(wf)
	if err != nil || peak.VMs != 4 {
		t.Fatalf("exit cleanup became a barrier: %+v %v", peak, err)
	}
}

func TestUnknownVMIGraphUsesSum(t *testing.T) {
	for _, kind := range []string{"cycle", "unknown-task", "or", "when", "fanout", "recursive", "retry", "exit-create", "task-exit", "template-defaults", "too-large", "mixed-depends", "continue-on"} {
		t.Run(kind, func(t *testing.T) {
			wf := phaseWorkflow([]wfv1.DAGTask{vmTask("a", "create-0"), vmTask("da", "delete-0", "a"), vmTask("b", "create-1", "da"), vmTask("db", "delete-1", "b"), vmTask("c", "create-2", "db")})
			dag := wf.Spec.Templates[len(wf.Spec.Templates)-1].DAG
			switch kind {
			case "cycle":
				dag.Tasks[0].Dependencies = []string{"c"}
			case "unknown-task":
				dag.Tasks[2].Dependencies = []string{"missing"}
			case "or":
				dag.Tasks[2].Dependencies = nil
				dag.Tasks[2].Depends = "da.Succeeded || a.Succeeded"
			case "when":
				dag.Tasks[1].When = "false"
			case "fanout":
				dag.Tasks[0].WithParam = "{{workflow.parameters.items}}"
			case "recursive":
				dag.Tasks[0].Template = "main"
			case "retry":
				wf.Spec.RetryStrategy = &wfv1.RetryStrategy{}
			case "exit-create":
				wf.Spec.OnExit = "create-0"
			case "task-exit":
				dag.Tasks[0].OnExit = "delete-0"
			case "template-defaults":
				wf.Spec.TemplateDefaults = &wfv1.Template{}
			case "too-large":
				for i := range maxVMIGraphNodes {
					dag.Tasks = append(dag.Tasks, vmTask(fmt.Sprintf("extra-%d", i), "get-0"))
				}
			case "continue-on":
				dag.Tasks[1].ContinueOn = &wfv1.ContinueOn{Failed: true}
			case "mixed-depends":
				dag.Tasks[2].Depends = "da.Succeeded"

			}
			sum, _ := SummedVMIDemand(wf)
			peak, err := DeclaredVMIDemand(wf)
			if err != nil || peak != sum {
				t.Fatalf("unknown %s earned credits: %+v sum=%+v %v", kind, peak, sum, err)
			}
		})
	}
}

// Exhaustive subsets provide an independent oracle for the weighted flow.
func TestWeightedVMIAntichainAgainstExhaustiveSubsets(t *testing.T) {
	for seed := range 64 {
		n := 6
		before := make([][]bool, n)
		weights := []int64{1, 3, 2, 5, 1, 4}
		for i := range n {
			before[i] = make([]bool, n)
			for j := i + 1; j < n; j++ {
				before[i][j] = (seed>>(uint((i+j)%6)))&1 != 0
			}
		}
		for k := range n {
			for i := range n {
				for j := range n {
					before[i][j] = before[i][j] || (before[i][k] && before[k][j])
				}
			}
		}
		var want int64
		for mask := range 1 << n {
			valid := true
			var w int64
			for i := range n {
				if mask&(1<<i) == 0 {
					continue
				}
				w += weights[i]
				for j := range n {
					if mask&(1<<j) != 0 && before[i][j] {
						valid = false
					}
				}
			}
			if valid {
				want = max(want, w)
			}
		}
		if got := maxWeightedVMAntichain(before, weights); got != want {
			t.Fatalf("seed=%d got=%d want=%d", seed, got, want)
		}
	}
}

func TestRepresentativeVMIPhasesFitExistingGrant(t *testing.T) {
	source, err := os.ReadFile("testdata/vmi-phases.yaml")
	if err != nil {
		t.Fatal(err)
	}
	wf, err := Decode(source)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := SummedVMIDemand(wf)
	if err != nil || sum != (VMIDemand{VMs: 4, Cores: 8, MemoryBytes: 24 << 30}) {
		t.Fatal(sum, err)
	}
	peak, err := DeclaredVMIDemand(wf)
	if err != nil || peak != (VMIDemand{VMs: 2, Cores: 4, MemoryBytes: 12 << 30}) {
		t.Fatal(peak, err)
	}
	if err := Admit(wf, Policy{KubeVirt: true, ContainerDiskPrefixes: []string{"quay.io/example/"}, RunnerImagePrefixes: []string{"busybox:"}}); err != nil {
		t.Fatalf("phase demand should fit unchanged default grant: %v", err)
	}
}

func TestVMIPeakNestedStepsAndSuccessfulDepends(t *testing.T) {
	wf := phaseWorkflow([]wfv1.DAGTask{vmTask("a", "create-0"), vmTask("da", "delete-0", "a"), vmTask("b", "create-1", "da"), vmTask("db", "delete-1", "b"), vmTask("c", "create-2", "db")})
	dag := wf.Spec.Templates[len(wf.Spec.Templates)-1].DAG
	for i := range dag.Tasks {
		if len(dag.Tasks[i].Dependencies) > 0 {
			dag.Tasks[i].Depends = strings.Join(dag.Tasks[i].Dependencies, ".Succeeded && ") + ".Succeeded"
			dag.Tasks[i].Dependencies = nil
		}
	}
	wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: "outer", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "nested", Template: "main"}}}}})
	wf.Spec.Entrypoint = "outer"
	peak, err := DeclaredVMIDemand(wf)
	if err != nil || peak.VMs != 1 {
		t.Fatal(peak, err)
	}
}
