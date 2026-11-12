package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func TestSchedulingCPUAllocatableHeadroomAndStepCap(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker"}, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("12")}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	client := fake.NewClientset(node)
	budget, cap, err := schedulingCPU(t.Context(), client, 20, 25)
	if err != nil || budget != 9600 || cap != 3000 {
		t.Fatalf("budget=%d cap=%d err=%v", budget, cap, err)
	}
	if _, _, err = schedulingCPU(t.Context(), fake.NewClientset(), 20, 25); err == nil {
		t.Fatal("unknown capacity opened admission")
	}
}

func TestSchedulingCPUExcludesTaintsAndSumsEligibleNodes(t *testing.T) {
	node := func(name, cpu string) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	}
	a, b, c, d, e, f := node("a", "4"), node("b", "12"), node("c", "100"), node("d", "100"), node("e", "100"), node("f", "100")
	c.Spec.Taints = []corev1.Taint{{Key: "dedicated", Effect: corev1.TaintEffectNoSchedule}}
	d.Spec.Taints = []corev1.Taint{{Key: "dedicated", Effect: corev1.TaintEffectNoExecute}}
	e.Spec.Unschedulable = true
	f.Status.Conditions[0].Status = corev1.ConditionFalse
	budget, cap, err := schedulingCPU(t.Context(), fake.NewClientset(a, b, c, d, e, f), 20, 25)
	if err != nil || budget != 12800 || cap != 3000 {
		t.Fatalf("budget=%d cap=%d err=%v", budget, cap, err)
	}
}
func TestSchedulingCPUOffSkipsNodeRead(t *testing.T) {
	options := serveOptions{}
	if _, err := schedulingKubernetesClient(t.Context(), &rest.Config{Host: "http://127.0.0.1:1"}, &options); err != nil {
		t.Fatal(err)
	}
	if options.admissionCPUMilli != 0 || options.stepCPURequestMaxMilli != 0 {
		t.Fatal(options)
	}
}
