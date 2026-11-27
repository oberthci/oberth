package ordering_test

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultpreemption"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/noderesources"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
)

// This separate module imports the real scheduler plugins. Product code has no
// dependency on Kubernetes scheduling internals and never calls these plugins.
func TestNativePodOrderAndNoPreemption(t *testing.T) {
	start := time.Unix(100, 0)
	queued := func(name string, priority int32, offset time.Duration) *framework.QueuedPodInfo {
		policy := corev1.PreemptNever
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.PodSpec{Priority: &priority, PreemptionPolicy: &policy}}
		info, err := framework.NewPodInfo(pod)
		if err != nil {
			t.Fatal(err)
		}
		return &framework.QueuedPodInfo{PodInfo: info, Timestamp: start.Add(offset)}
	}
	lint := queued("lint", 0, 0)
	qualification := queued("qualification", 10000, time.Second)
	promotion := queued("promotion", 20000, 2*time.Second)
	release := queued("release", 30000, 3*time.Second)
	order := &queuesort.PrioritySort{}
	if !order.Less(release, promotion) || !order.Less(promotion, qualification) || !order.Less(qualification, lint) {
		t.Fatal("native active queue ignored priorities")
	}
	older := queued("older-release", 30000, 0)
	if !order.Less(older, release) || order.Less(release, older) {
		t.Fatal("equal-priority Pods lost queue timestamp order")
	}
	preemption := &defaultpreemption.DefaultPreemption{}
	for _, pod := range []*framework.QueuedPodInfo{lint, qualification, promotion, release} {
		eligible, reason := preemption.PodEligibleToPreemptOthers(t.Context(), pod.PodInfo.Pod, nil)
		if eligible || reason != "not eligible due to preemptionPolicy=Never." {
			t.Fatal("nonpreempting class can initiate preemption", eligible, reason)
		}
	}
	t.Log("native active queue: release > promotion > qualification > lint; all four are ineligible to preempt")
}

func TestNativeResourceFitWaitsForCapacity(t *testing.T) {
	requests := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}
	running := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "running-lint"}, Spec: corev1.PodSpec{
		Containers: []corev1.Container{{Name: "main", Resources: corev1.ResourceRequirements{Requests: requests}}},
	}}
	release := running.DeepCopy()
	release.Name = "waiting-release"
	priority, policy := int32(30000), corev1.PreemptNever
	release.Spec.Priority, release.Spec.PreemptionPolicy = &priority, &policy
	node := &corev1.Node{Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourcePods: resource.MustParse("10"),
	}}}
	occupied, free := framework.NewNodeInfo(running), framework.NewNodeInfo()
	occupied.SetNode(node)
	free.SetNode(node)
	fit, state := &noderesources.Fit{}, framework.NewCycleState()
	if _, status := fit.PreFilter(t.Context(), state, release, nil); !status.IsSuccess() {
		t.Fatal(status)
	}
	if status := fit.Filter(t.Context(), state, release, occupied); status.IsSuccess() || !strings.Contains(status.Message(), "Insufficient cpu") {
		t.Fatal("higher priority did not wait for native CPU feasibility", status)
	}
	if status := fit.Filter(t.Context(), state, release, free); !status.IsSuccess() {
		t.Fatal("native CPU capacity did not become feasible", status)
	}
}
