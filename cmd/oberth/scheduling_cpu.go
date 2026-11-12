package main

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// Sum eligible nodes for admission and cap each step against the largest node.
// Restart the server after changing the node pool to refresh this snapshot.
func schedulingCPU(ctx context.Context, kube kubernetes.Interface, headroom, stepPercent int64) (int64, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, 0, fmt.Errorf("read allocatable CPU for admission: %w", err)
	}
	var cpu, largest int64
	for _, node := range nodes.Items {
		ready := false
		for _, c := range node.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if !ready || node.Spec.Unschedulable || schedulingTainted(node) {
			continue
		}
		n := node.Status.Allocatable.Cpu().MilliValue()
		if n > 0 {
			cpu += n
			largest = max(largest, n)
		}
	}
	if cpu <= 0 {
		return 0, 0, fmt.Errorf("CPU admission requires a Ready schedulable node with positive allocatable CPU")
	}
	budget := cpu * (100 - headroom) / 100
	if budget <= 0 {
		return 0, 0, fmt.Errorf("no allocatable CPU remains after admission headroom")
	}
	return budget, max(int64(1), largest*stepPercent/100), nil
}

func schedulingKubernetesClient(ctx context.Context, config *rest.Config, options *serveOptions) (kubernetes.Interface, error) {
	kube, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	if !options.admissionCPUEnabled {
		return kube, nil
	}
	options.admissionCPUPool = config.Host
	options.admissionCPUMilli, options.stepCPURequestMaxMilli, err = schedulingCPU(ctx, kube, options.admissionCPUHeadroomPercent, options.stepCPURequestPercent)
	if err != nil {
		return nil, err
	}
	return kube, nil
}

func schedulingTainted(node corev1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute {
			return true
		}
	}
	return false
}

func buildSchedulingArgoTargets(ctx context.Context, policy *argoTargetPolicy, options serveOptions) ([]app.ExecutionTarget, error) {
	targets, err := buildArgoTarget(policy)
	if err != nil {
		return nil, err
	}
	for i := range targets {
		target := &targets[i]
		target.Config.CPUAdmissionDisabled = !options.admissionCPUEnabled
		if !options.admissionCPUEnabled {
			continue
		}
		budget, cap, err := schedulingCPU(ctx, target.Kubernetes, options.admissionCPUHeadroomPercent, options.stepCPURequestPercent)
		if err != nil {
			return nil, fmt.Errorf("execution target %s: %w", target.ID, err)
		}
		target.Config.CPUAdmission = argoworkflow.CPUAdmission{Pool: target.Transport.Host, Milli: budget}
		target.Config.StepCPURequestMaxMilli = cap
	}
	return targets, nil
}
