package main

import (
	"errors"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func cpuTestNode(name, cpu string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
}
func TestCPUCapacityStartsHeldAndRecoversAfterNodeRefresh(t *testing.T) {
	node := cpuTestNode("worker", "12")
	node.Spec.Unschedulable = true
	client := fake.NewClientset(node)
	var denied atomic.Bool
	client.PrependReactor("list", "nodes", func(ktesting.Action) (bool, runtime.Object, error) {
		if denied.Load() {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", errors.New("denied"))
		}
		return false, nil, nil
	})
	monitor := newCPUCapacity(t.Context(), client, "cluster", "", 20, 25)
	if p := monitor.snapshot(t.Context()); p.HoldReason == "" || p.Milli != 0 {
		t.Fatal("cordoned node should hold admission", p)
	}
	node.Spec.Unschedulable = false
	if _, err := client.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	monitor.refresh(t.Context())
	if p := monitor.snapshot(t.Context()); p.Milli != 9600 || p.NodeMilli != 12000 || p.StepMaxMilli != 3000 || p.HoldReason != "" {
		t.Fatal("capacity did not refresh", p)
	}
	denied.Store(true)
	monitor.refresh(t.Context())
	if p := monitor.snapshot(t.Context()); p.HoldReason == "" || p.Milli != 0 {
		t.Fatal("unavailable read should hold without abort", p)
	}
	denied.Store(false)
	node.Status.Allocatable[corev1.ResourceCPU] = resource.MustParse("8")
	if _, err := client.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	monitor.refresh(t.Context())
	if p := monitor.snapshot(t.Context()); p.Milli != 6400 || p.StepMaxMilli != 2000 {
		t.Fatal("node resize required restart", p)
	}
}
func TestCPUCapacityPinnedTargetReadsOnlyItsNode(t *testing.T) {
	small, big := cpuTestNode("selected", "4"), cpuTestNode("other", "64")
	small.Spec.Taints = []corev1.Taint{{Key: "dedicated", Value: "execution", Effect: corev1.TaintEffectNoSchedule}}
	client := fake.NewClientset(small, big)
	client.PrependReactor("list", "nodes", func(ktesting.Action) (bool, runtime.Object, error) {
		t.Error("pinned target listed all nodes")
		return true, nil, errors.New("list denied")
	})
	monitor := newCPUCapacity(t.Context(), client, "target", "selected", 20, 25, corev1.Toleration{Key: "dedicated", Value: "execution", Operator: corev1.TolerationOpEqual, Effect: corev1.TaintEffectNoSchedule})
	if p := monitor.snapshot(t.Context()); p.Milli != 3200 || p.NodeMilli != 4000 || p.StepMaxMilli != 1000 {
		t.Fatal("target inherited other node capacity", p)
	}
	client.PrependReactor("get", "nodes", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("get denied") })
	monitor.refresh(t.Context())
	if p := monitor.snapshot(t.Context()); p.HoldReason == "" {
		t.Fatal("target permission failure should hold", p)
	}
}
