package app

import (
	"context"
	"fmt"
	"math"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type allocatableNodeReader interface {
	List(context.Context, metav1.ListOptions) (*corev1.NodeList, error)
}

// AdmissionCapacityProbe is advisory only. It never changes a scheduler budget
// or readiness. Allocatable is an upper bound: per-workflow placement, taints,
// and competing workloads can further reduce usable capacity.
type AdmissionCapacityProbe struct {
	Nodes                 allocatableNodeReader
	CPUMilli, MemoryBytes int64
	OvercommitFactor      float64
	Log                   func(string, ...any)
}

type AdmissionCapacityStatus struct {
	Available              bool     `json:"available"`
	Nodes                  int      `json:"nodes"`
	DeclaredCPUMilli       int64    `json:"declared_cpu_milli"`
	DeclaredMemoryBytes    int64    `json:"declared_memory_bytes"`
	AllocatableCPUMilli    int64    `json:"allocatable_cpu_milli"`
	AllocatableMemoryBytes int64    `json:"allocatable_memory_bytes"`
	OvercommitFactor       float64  `json:"overcommit_factor"`
	Warnings               []string `json:"warnings"`
}

// Check bounds the Kubernetes read independently of the HTTP caller. The same
// check runs at startup and on unscoped system_status requests.
func (probe AdmissionCapacityProbe) Check(ctx context.Context) *AdmissionCapacityStatus {
	status := &AdmissionCapacityStatus{DeclaredCPUMilli: probe.CPUMilli, DeclaredMemoryBytes: probe.MemoryBytes, OvercommitFactor: probe.OvercommitFactor, Warnings: []string{}}
	if status.OvercommitFactor == 0 {
		status.OvercommitFactor = 1
	}
	warn := func(message string) {
		status.Warnings = append(status.Warnings, message)
		if probe.Log != nil {
			probe.Log("WARN: admission capacity: %s", message)
		}
	}
	if probe.Nodes == nil {
		warn("cluster allocatable unavailable: no node reader")
		return status
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	nodes, err := probe.Nodes.List(ctx, metav1.ListOptions{})
	if err != nil {
		warn(fmt.Sprintf("cluster allocatable unavailable: %v", err))
		return status
	}
	for _, node := range nodes.Items {
		ready := false
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady {
				ready = condition.Status == corev1.ConditionTrue
			}
		}
		if node.Spec.Unschedulable || !ready {
			continue
		}
		cpu, memory := node.Status.Allocatable.Cpu().MilliValue(), node.Status.Allocatable.Memory().Value()
		if cpu < 0 || memory < 0 || cpu > math.MaxInt64-status.AllocatableCPUMilli || memory > math.MaxInt64-status.AllocatableMemoryBytes {
			warn("cluster allocatable unavailable: invalid node resource totals")
			return status
		}
		status.Nodes++
		status.AllocatableCPUMilli += cpu
		status.AllocatableMemoryBytes += memory
	}
	status.Available = true
	for _, budget := range []struct {
		name                  string
		declared, allocatable int64
	}{
		{"CPU (millicores)", probe.CPUMilli, status.AllocatableCPUMilli},
		{"memory (bytes)", probe.MemoryBytes, status.AllocatableMemoryBytes},
	} {
		if float64(budget.declared) > float64(budget.allocatable)*status.OvercommitFactor {
			warn(fmt.Sprintf("declared %s budget %d exceeds cluster allocatable %d × overcommit factor %g; review the budget or explicitly opt in with admission.overcommitFactor / --admission-overcommit-factor (admission is unchanged)", budget.name, budget.declared, budget.allocatable, status.OvercommitFactor))
		}
	}
	return status
}
