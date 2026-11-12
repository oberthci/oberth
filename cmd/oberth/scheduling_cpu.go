package main

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func schedulingCPU(ctx context.Context, kube kubernetes.Interface, nodeName string, headroom, stepPercent int64, tolerations ...corev1.Toleration) (argoworkflow.CPUAdmission, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var nodes []corev1.Node
	if nodeName != "" {
		node, err := kube.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return argoworkflow.CPUAdmission{}, err
		}
		nodes = []corev1.Node{*node}
	} else {
		list, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return argoworkflow.CPUAdmission{}, err
		}
		nodes = list.Items
	}
	var total, largest int64
	for _, node := range nodes {
		ready := false
		for _, c := range node.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if !ready || node.Spec.Unschedulable || schedulingTainted(node, tolerations...) {
			continue
		}
		n := node.Status.Allocatable.Cpu().MilliValue()
		if n > 0 {
			total += n
			largest = max(largest, n)
		}
	}
	p := argoworkflow.CPUAdmission{Milli: total * (100 - headroom) / 100, NodeMilli: largest, StepMaxMilli: largest * stepPercent / 100}
	if p.Milli <= 0 {
		p.HoldReason = "CPU admission held: no eligible Ready schedulable node"
	}
	return p, nil
}

func schedulingKubernetesClient(ctx context.Context, config *rest.Config, options *serveOptions) (kubernetes.Interface, error) {
	kube, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	if !options.admissionCPUEnabled {
		return kube, nil
	}
	monitor := newCPUCapacity(ctx, kube, config.Host, "", options.admissionCPUHeadroomPercent, options.stepCPURequestPercent)
	options.cpuCapacity = monitor.snapshot
	p := monitor.snapshot(ctx)
	options.admissionCPUPool, options.admissionCPUMilli, options.stepCPURequestMaxMilli = p.Pool, p.Milli, p.StepMaxMilli

	return kube, nil
}

func schedulingTainted(node corev1.Node, tolerations ...corev1.Toleration) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute {
			allowed := false
			for _, t := range tolerations {
				if t.Key == taint.Key && t.Value == taint.Value && t.Effect == taint.Effect && t.Operator == corev1.TolerationOpEqual {
					allowed = true
				}
			}
			if !allowed {
				return true
			}
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
		monitor := newCPUCapacity(ctx, target.Kubernetes, target.Transport.Host, target.Config.ExecutionPlacement.CPUNodeName(), options.admissionCPUHeadroomPercent, options.stepCPURequestPercent, target.Config.ExecutionPlacement.CPUTolerations()...)
		target.Config.CPUCapacity = monitor.snapshot
		target.Config.CPUAdmission = monitor.snapshot(ctx)
		target.Config.StepCPURequestMaxMilli = target.Config.CPUAdmission.StepMaxMilli
	}
	return targets, nil
}
