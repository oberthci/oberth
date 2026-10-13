package argojob

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCheckKVMAllocatableNodeAvailable(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				kvmResourceName: resource.MustParse("1000"),
			},
		},
	})
	if err := CheckKVMAllocatable(context.Background(), client); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestCheckKVMAllocatableNoNodes(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset()
	err := CheckKVMAllocatable(context.Background(), client)
	if err == nil {
		t.Fatal("expected error for no nodes")
	}
	if !strings.Contains(err.Error(), "no schedulable node") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckKVMAllocatableNoKVMDevice(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("4"),
			},
		},
	})
	err := CheckKVMAllocatable(context.Background(), client)
	if err == nil {
		t.Fatal("expected error for no KVM device")
	}
	if !strings.Contains(err.Error(), "no schedulable node") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckKVMAllocatableUnschedulableNode(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Spec:       corev1.NodeSpec{Unschedulable: true},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				kvmResourceName: resource.MustParse("1000"),
			},
		},
	})
	err := CheckKVMAllocatable(context.Background(), client)
	if err == nil {
		t.Fatal("expected error for unschedulable node")
	}
}

func TestCheckKVMAllocatableNilClient(t *testing.T) {
	t.Parallel()
	err := CheckKVMAllocatable(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for nil client")
	}
}
