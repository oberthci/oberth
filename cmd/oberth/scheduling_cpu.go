package main

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Use the smallest eligible node as a conservative single-node packing pool.
// This avoids promising aggregate capacity that cannot place a whole Pod.
// Restart the server after changing the node pool to refresh this snapshot.
func schedulingCPU(ctx context.Context, kube kubernetes.Interface, headroom, stepPercent int64) (int64, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, 0, fmt.Errorf("read allocatable CPU for admission: %w", err)
	}
	var cpu int64
	for _, node := range nodes.Items {
		ready := false
		for _, c := range node.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if !ready || node.Spec.Unschedulable {
			continue
		}
		n := node.Status.Allocatable.Cpu().MilliValue()
		if n > 0 && (cpu == 0 || n < cpu) {
			cpu = n
		}
	}
	if cpu <= 0 {
		return 0, 0, fmt.Errorf("CPU admission requires a Ready schedulable node with positive allocatable CPU")
	}
	budget := cpu * (100 - headroom) / 100
	if budget <= 0 {
		return 0, 0, fmt.Errorf("no allocatable CPU remains after admission headroom")
	}
	return budget, max(int64(1), cpu*stepPercent/100), nil
}

func schedulingKubernetesClient(ctx context.Context, config *rest.Config, options *serveOptions) (kubernetes.Interface, error) {
	kube, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	options.admissionCPUMilli, options.stepCPURequestMaxMilli, err = schedulingCPU(ctx, kube, options.admissionCPUHeadroomPercent, options.stepCPURequestPercent)
	if err != nil {
		return nil, err
	}
	return kube, nil
}
