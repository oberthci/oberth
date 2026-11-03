package argoworkflow

import (
	"errors"
	"reflect"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestContainerDemandReservesMemoryRequest(t *testing.T) {
	for _, tc := range []struct {
		name, request, limit string
		inherited            bool
		wantErr              error
	}{
		{name: "request-only", request: "2Gi", wantErr: ErrMemoryUnbounded},
		{name: "request-and-limit", request: "2Gi", limit: "3Gi"},
		{name: "inherited", request: "2Gi", limit: "3Gi", inherited: true},
		{name: "limit-only", limit: "3Gi", wantErr: errPodUndeclared},
		{name: "undeclared", wantErr: errPodUndeclared},
		{name: "zero-request", request: "0", limit: "3Gi", wantErr: errPodUndeclared},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &corev1.Container{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}, Limits: corev1.ResourceList{}}}
			if tc.request != "" {
				c.Resources.Requests[corev1.ResourceMemory] = resource.MustParse(tc.request)
			}
			if tc.limit != "" {
				c.Resources.Limits[corev1.ResourceMemory] = resource.MustParse(tc.limit)
			}
			var defaults *corev1.Container
			if tc.inherited {
				defaults = c.DeepCopy()
				c.Resources = corev1.ResourceRequirements{}
			}
			before := c.DeepCopy()
			d, err := containerDemand(c, defaults)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && (d.MemoryBytes != 2<<30 || d.CPUMilli != 100 || d.Pods != 1) {
				t.Fatalf("demand = %+v, want requests only", d)
			}
			if !reflect.DeepEqual(c, before) {
				t.Fatal("container declarations changed")
			}
		})
	}
}

func TestRunDemandUsesConcurrentMemoryRequests(t *testing.T) {
	a, b, c := declaredLeaf("a", "1", "2Gi"), declaredLeaf("b", "1", "3Gi"), declaredLeaf("c", "1", "4Gi")
	for _, leaf := range []*wfv1.Template{&a, &b, &c} {
		leaf.Container.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("8Gi")
	}
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "root", Templates: []wfv1.Template{
		{Name: "root", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "a", Template: "a"}, {Name: "b", Template: "b"}, {Name: "c", Template: "c", Dependencies: []string{"a", "b"}}}}}, a, b, c,
	}}}
	before := wf.DeepCopy()
	d, err := DeclaredHostPeak(wf, RunTriggerBranch)
	if err != nil || d == nil || d.MemoryBytes != 5<<30 || d.Pods != 2 {
		t.Fatalf("run demand = %+v, %v; want concurrent 5Gi requests", d, err)
	}
	if !reflect.DeepEqual(wf, before) {
		t.Fatal("workflow declarations changed")
	}
	delete(wf.Spec.Templates[1].Container.Resources.Requests, corev1.ResourceMemory)
	if d, err := DeclaredHostPeak(wf, RunTriggerBranch); err != nil || d != nil {
		t.Fatalf("undeclared step gained numeric credit: %+v, %v", d, err)
	}
}
