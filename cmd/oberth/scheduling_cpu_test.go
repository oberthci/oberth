package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
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
