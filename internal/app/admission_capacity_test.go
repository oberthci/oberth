package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type capacityNodes struct {
	nodes []corev1.Node
	err   error
	calls int
}

func (n *capacityNodes) List(ctx context.Context, _ metav1.ListOptions) (*corev1.NodeList, error) {
	n.calls++
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("missing bounded deadline")
	}
	return &corev1.NodeList{Items: n.nodes}, n.err
}
func capacityNode(cpu, memory string) corev1.Node {
	return corev1.Node{Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(memory)}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
}
func TestAdmissionCapacitySanity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cpu      int64
		memory   int64
		factor   float64
		warnings int
	}{
		{"equal", 8000, 64 << 30, 1, 0}, {"memory over", 8000, 96 << 30, 1, 1}, {"cpu over", 12000, 64 << 30, 1, 1},
		{"both over", 12000, 96 << 30, 1, 2}, {"opt in", 12000, 96 << 30, 1.5, 0}, {"opt in still exceeded", 13000, 100 << 30, 1.5, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			excluded := capacityNode("100", "1000Gi")
			excluded.Spec.Unschedulable = true
			unready := capacityNode("100", "1000Gi")
			unready.Status.Conditions = nil
			nodes := &capacityNodes{nodes: []corev1.Node{capacityNode("4", "32Gi"), capacityNode("4", "32Gi"), excluded, unready}}
			var messages []string
			probe := AdmissionCapacityProbe{Nodes: nodes, CPUMilli: tc.cpu, MemoryBytes: tc.memory, OvercommitFactor: tc.factor, Log: func(format string, args ...any) { messages = append(messages, fmt.Sprintf(format, args...)) }}
			status := probe.Check(t.Context())
			if !status.Available || status.AllocatableCPUMilli != 8000 || status.AllocatableMemoryBytes != 64<<30 || len(status.Warnings) != tc.warnings || len(messages) != tc.warnings {
				t.Fatalf("sanity: %+v logs=%v", status, messages)
			}
			if status.DeclaredCPUMilli != tc.cpu || status.DeclaredMemoryBytes != tc.memory || status.OvercommitFactor != tc.factor {
				t.Fatal("probe rewrote budgets")
			}
		})
	}
}
func TestAdmissionCapacityUnavailableIsAdvisoryAndUnscoped(t *testing.T) {
	nodes := &capacityNodes{err: errors.New("nodes list denied")}
	probe := AdmissionCapacityProbe{Nodes: nodes, CPUMilli: 8000, MemoryBytes: 96 << 30, OvercommitFactor: 1}
	health := Health{Configured: func(context.Context, []model.Upstream) error { return nil }, Cluster: func(context.Context) error { return nil }, VCS: func(context.Context, model.Upstream) error { return nil }, Audit: func(context.Context) error { return nil }, Store: fakeHealthStore{upstreams: []model.Upstream{{ID: 1, Name: "example"}}}, AdmissionCapacity: probe.Check}
	if err := health.Ready(t.Context()); err != nil {
		t.Fatalf("advisory blocked readiness: %v", err)
	}
	value, err := health.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	status := value.(HealthStatus).AdmissionCapacity
	if status.Available || len(status.Warnings) != 1 || !strings.Contains(status.Warnings[0], "nodes list denied") {
		t.Fatalf("missing unavailable warning: %+v", status)
	}
	before := nodes.calls
	value, err = health.ScopedStatus(t.Context(), api.Actor{Identity: "reader", Scope: []string{"example"}})
	if err != nil {
		t.Fatal(err)
	}
	if nodes.calls != before || value.(HealthStatus).AdmissionCapacity != nil {
		t.Fatal("scoped status exposes cluster capacity")
	}
	nodes.err = nil
	status = probe.Check(t.Context())
	if !status.Available || len(status.Warnings) != 2 {
		t.Fatalf("empty cluster: %+v", status)
	}
}
