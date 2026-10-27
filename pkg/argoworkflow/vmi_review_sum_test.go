package argoworkflow

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func TestReviewDemandCapsEveryDimensionAtTemplateSum(t *testing.T) {
	for _, template := range []string{"create-a", "create-b"} {
		for _, complete := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/complete=%v", template, complete), func(t *testing.T) {
				wf, binding, policy := vmiSoundnessWorkflow(t)
				a := runVMITemplate(wf, "create-a")
				a.Resource.Manifest = strings.ReplaceAll(a.Resource.Manifest, "cores: 2", "cores: 4")
				a.Resource.Manifest = strings.ReplaceAll(a.Resource.Manifest, "memory: 6Gi", "memory: 2Gi")
				b := runVMITemplate(wf, "create-b")
				b.Resource.Manifest = strings.ReplaceAll(b.Resource.Manifest, "cores: 2", "cores: 1")
				b.Resource.Manifest = strings.ReplaceAll(b.Resource.Manifest, `cpu: "2"`, `cpu: "1"`)
				b.Resource.Manifest = strings.ReplaceAll(b.Resource.Manifest, "memory: 6Gi", "memory: 8Gi")
				wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
					{Name: "first", Template: template}, {Name: "second", Template: template},
				}}})
				source, err := json.Marshal(wf)
				if err != nil {
					t.Fatal(err)
				}
				if !complete {
					binding = RunSourceBinding{}
				}
				r, err := ResolveRunSource(source, nil, binding)
				if err != nil {
					t.Fatal(err)
				}
				sum, err := SummedVMIDemand(wf)
				if err != nil || sum != (VMIDemand{VMs: 2, Cores: 5, MemoryBytes: 10 << 30}) {
					t.Fatalf("weighted template fixture changed: %+v %v", sum, err)
				}
				want := VMIDemand{VMs: 2, Cores: 5, MemoryBytes: 4 << 30}
				if template == "create-b" {
					want = VMIDemand{VMs: 2, Cores: 2, MemoryBytes: 10 << 30}
				}
				if !complete {
					// Legacy projection cannot ignore the unused create template,
					// so it retains the sum without run-aware reachability credits.
					want = sum
				}
				got, err := r.demand()
				if err != nil || got != want {
					t.Fatalf("each dimension must use min(projection, sum): got %+v %v, want %+v", got, err, want)
				}
				if _, err := r.Receipt(policy); err != nil {
					t.Fatalf("weighted fixture must remain policy-valid: %v", err)
				}
			})
		}
	}
}

func TestReviewRepeatedTemplateDoesNotInflateVMCount(t *testing.T) {
	wf, binding, policy := vmiSoundnessWorkflow(t)
	create := runVMITemplate(wf, "create-a").DeepCopy()
	wf.Spec.Templates = []wfv1.Template{*create, {Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
		{Name: "first", Template: create.Name}, {Name: "second", Template: create.Name}, {Name: "third", Template: create.Name},
	}}}}
	policy.VMIBudget.MaxVMs = 1
	checkVMISoundnessDemand(t, wf, binding, policy, 1)
}
