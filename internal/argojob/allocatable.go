package argojob

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const kvmResourceName corev1.ResourceName = "devices.kubevirt.io/kvm"

// CheckKVMAllocatable verifies that at least one node in the cluster
// advertises allocatable devices.kubevirt.io/kvm capacity. This is an
// infrastructure pre-check that fails fast before submitting a workflow
// that requires KVM, rather than waiting for the scheduler to report
// Unschedulable after workflow creation.
//
// The check is read-only and requires only get/list on nodes.
func CheckKVMAllocatable(ctx context.Context, client kubernetes.Interface) error {
	if client == nil {
		return fmt.Errorf("argojob: Kubernetes client is required for KVM allocatable check")
	}

	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		Limit: 64,
	})
	if err != nil {
		return fmt.Errorf("argojob: list nodes for KVM allocatable check: %w", err)
	}

	for i := range nodes.Items {
		node := &nodes.Items[i]
		// Check if the node is schedulable.
		if node.Spec.Unschedulable {
			continue
		}
		// Check allocatable resources for KVM devices.
		if quantity, ok := node.Status.Allocatable[kvmResourceName]; ok {
			if quantity.Value() > 0 {
				return nil
			}
		}
	}

	return fmt.Errorf("argojob: no schedulable node has allocatable %s; the KubeVirt device plugin must be installed and advertising capacity", kvmResourceName)
}
