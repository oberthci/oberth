package argoworkflow

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

// The large case uses a small template pool, as repeated template invocations
// must not evade cost limits simply because the source document stays small.
func vmiCostWorkflow(t testing.TB, creates, deletes int) *wfv1.Workflow {
	t.Helper()
	source, _, _, _ := runVMIFixture(t)
	wf, err := Decode(source)
	if err != nil {
		t.Fatal(err)
	}
	create := runVMITemplate(wf, "create-a").DeepCopy()
	remove := runVMITemplate(wf, "delete-a").DeepCopy()
	selector := runVMITemplate(wf, "selector").DeepCopy()
	wf.Spec.Templates = []wfv1.Template{*create, *remove, *selector}
	wf.Spec.OnExit = ""
	main := wfv1.Template{Name: "main", DAG: &wfv1.DAGTemplate{}}
	for i := range (creates + 49) / 50 {
		group := wfv1.Template{Name: fmt.Sprintf("group-%d", i), DAG: &wfv1.DAGTemplate{}}
		for j := i * 50; j < min(creates, (i+1)*50); j++ {
			name := create.Name
			if creates <= 17 {
				unique := create.DeepCopy()
				unique.Name = fmt.Sprintf("create-%d", j)
				op, err := VMIOperation(unique.Resource)
				if err != nil {
					t.Fatal(err)
				}
				unique.Resource.Manifest = strings.ReplaceAll(unique.Resource.Manifest, op.Name, fmt.Sprintf("{{workflow.name}}-vm-%d", j))
				wf.Spec.Templates = append(wf.Spec.Templates, *unique)
				name = unique.Name
			}
			group.DAG.Tasks = append(group.DAG.Tasks, wfv1.DAGTask{Name: fmt.Sprintf("vm-%d", j), Template: name})
		}
		wf.Spec.Templates = append(wf.Spec.Templates, group)
		main.DAG.Tasks = append(main.DAG.Tasks, wfv1.DAGTask{Name: group.Name, Template: group.Name})
	}
	main.DAG.Tasks = append(main.DAG.Tasks, wfv1.DAGTask{Name: "select", Template: "selector"})
	for i := range deletes {
		main.DAG.Tasks = append(main.DAG.Tasks, wfv1.DAGTask{Name: fmt.Sprintf("delete-%d", i), Template: remove.Name, Depends: "select.Succeeded", When: fmt.Sprintf("{{=tasks['select'].outputs.parameters['partition'] == '%d'}}", i)})
	}
	wf.Spec.Templates = append(wf.Spec.Templates, main)
	return wf
}

func TestRunVMIProjectionScenarioWorkBound(t *testing.T) {
	for _, count := range []int{15, 16, 17, 200} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			deletes := 0
			if count == 200 {
				deletes = 32
			}
			wf := vmiCostWorkflow(t, count, deletes)
			peak, supported := runVMIPeak(wf, RunTriggerSchedule)
			// 15 creates fit the work budget; 16 overflow the shared cubic
			// scenario work budget; 17/200 hit the per-scenario create cap.
			if count == 15 {
				if !supported || peak.VMs != count {
					t.Fatalf("bounded proof lost: %+v %v", peak, supported)
				}
			} else if supported {
				t.Fatalf("unbounded scenario proof accepted: %+v", peak)
			}
		})
	}
}

func TestResolvedVMIDemandConcurrentImmutableSource(t *testing.T) {
	source, fragments, binding, policy := runVMIFixture(t)
	r, err := ResolveRunSource(source, fragments, binding)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 4 {
				demand, err := r.demand()
				if err != nil || demand != (VMIDemand{VMs: 2, Cores: 4, MemoryBytes: 12 << 30}) {
					t.Errorf("inconsistent demand: %+v %v", demand, err)
				}
			}
		})
	}
	wg.Wait()
	// Warming the cache must not bypass graph equality or policy admission.
	changed := r.Workflow()
	changed.Spec.Entrypoint = "create-a"
	if _, err := r.demandFor(changed); err == nil {
		t.Fatal("cached demand applied to a different graph")
	}
	policy.VMIBudget.MaxVMs = 1
	if _, err := r.Receipt(policy); err == nil {
		t.Fatal("cached demand bypassed the current grant")
	}
}

func BenchmarkRunVMIBoundedLargeProjection(b *testing.B) {
	wf := vmiCostWorkflow(b, 200, 32)
	b.ReportAllocs()
	for b.Loop() {
		if _, supported := runVMIPeak(wf, RunTriggerSchedule); supported {
			b.Fatal("large scenario did not fall back")
		}
	}
}

func BenchmarkResolvedVMIDemand(b *testing.B) {
	source, fragments, binding, _ := runVMIFixture(b)
	r, err := ResolveRunSource(source, fragments, binding)
	if err != nil {
		b.Fatal(err)
	}
	if _, err = r.demand(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if demand, err := r.demand(); err != nil || demand.VMs != 2 {
			b.Fatal(demand, err)
		}
	}
}

func TestResolvedVMIWorkFallbackUsesSum(t *testing.T) {
	wf := vmiCostWorkflow(t, 17, 0)
	_, _, binding, _ := runVMIFixture(t)
	source, err := json.Marshal(wf)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ResolveRunSource(source, nil, binding)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.demand()
	sum, sumErr := SummedVMIDemand(wf)
	if err != nil || sumErr != nil || got != sum {
		t.Fatalf("bounded fallback changed the sum: %+v/%+v %v/%v", got, sum, err, sumErr)
	}
}
