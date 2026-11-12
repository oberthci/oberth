package argoworkflow

import (
	"encoding/json"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

func TestExtendedResourcePeakFollowsWorkflowConcurrency(t *testing.T) {
	device := corev1.ResourceName("devices.example.test/worker")
	leaf := declaredLeaf("worker", "1", "128Mi")
	delete(leaf.Container.Resources.Requests, corev1.ResourceCPU)
	delete(leaf.Container.Resources.Requests, corev1.ResourceMemory)
	delete(leaf.Container.Resources.Limits, corev1.ResourceMemory)
	leaf.Container.Resources.Requests[device] = resource.MustParse("1")
	leaf.Container.Resources.Limits[device] = resource.MustParse("1")
	for _, tc := range []struct {
		name                         string
		serial                       bool
		workflowLimit, templateLimit int64
		want                         int64
	}{
		{name: "parallel", want: 2}, {name: "serial", serial: true, want: 1},
		{name: "workflow bound", workflowLimit: 1, want: 1}, {name: "template bound", templateLimit: 1, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tasks := []wfv1.DAGTask{{Name: "first", Template: "worker"}, {Name: "second", Template: "worker"}}
			if tc.serial {
				tasks[1].Dependencies = []string{"first"}
			}
			root := wfv1.Template{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: tasks}}
			if tc.templateLimit > 0 {
				root.Parallelism = ptr.To(tc.templateLimit)
			}
			wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "main", Templates: []wfv1.Template{root, leaf}}}
			if tc.workflowLimit > 0 {
				wf.Spec.Parallelism = ptr.To(tc.workflowLimit)
			}
			demand, err := DeclaredCapabilityPeak(wf)
			if err != nil || demand == nil {
				t.Fatalf("peak: %+v %v", demand, err)
			}
			encoded, err := json.Marshal(demand)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				ExtendedResources map[string]int64 `json:"extended_resources"`
			}
			if err = json.Unmarshal(encoded, &got); err != nil {
				t.Fatal(err)
			}
			if got.ExtendedResources[string(device)] != tc.want {
				t.Fatalf("extended peak=%v, want %d", got.ExtendedResources, tc.want)
			}
		})
	}
}

func TestCapabilityPeakIncludesExitAndConcurrentHookResources(t *testing.T) {
	device := corev1.ResourceName("devices.example.test/worker")
	main, handler := declaredLeaf("work", "1", "128Mi"), declaredLeaf("handler", "1", "128Mi")
	main.Container.Resources.Requests[device] = resource.MustParse("1")
	main.Container.Resources.Limits[device] = resource.MustParse("1")
	handler.Container.Resources.Requests[device] = resource.MustParse("2")
	handler.Container.Resources.Limits[device] = resource.MustParse("2")
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "work", OnExit: "handler", Templates: []wfv1.Template{main, handler}}}
	grant := &Capability{ExtendedResources: map[string]int64{string(device): 2}}
	if err := validateExtendedPeak(wf, grant); err != nil {
		t.Fatal("sequential exit double-counted resources", err)
	}
	wf.Spec.OnExit = ""
	wf.Spec.Hooks = wfv1.LifecycleHooks{"running": {Template: "handler", Expression: "workflow.status == 'Running'"}}
	if err := validateExtendedPeak(wf, grant); err == nil {
		t.Fatal("concurrent hook escaped extended-resource grant")
	}
	grant.ExtendedResources[string(device)] = 3
	if err := validateExtendedPeak(wf, grant); err != nil {
		t.Fatal("sufficient concurrent-hook grant rejected", err)
	}
}
