package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/argojob"

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
	monitor := newCPUCapacity(t.Context(), client, "cluster", "", nil, 20, 25)
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
	monitor.refreshAt(t.Context(), monitor.lastGood.Add(time.Minute))
	if p := monitor.snapshot(t.Context()); p.Milli != 9600 || p.HoldReason != "" {
		t.Fatal("transient read lost last good capacity", p)
	}
	monitor.refreshAt(t.Context(), monitor.lastGood.Add(cpuCapacityGrace))
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
	monitor := newCPUCapacity(t.Context(), client, "target", "selected", nil, 20, 25, corev1.Toleration{Key: "dedicated", Value: "execution", Operator: corev1.TolerationOpEqual, Effect: corev1.TaintEffectNoSchedule})
	if p := monitor.snapshot(t.Context()); p.Milli != 3200 || p.NodeMilli != 4000 || p.StepMaxMilli != 1000 {
		t.Fatal("target inherited other node capacity", p)
	}
	client.PrependReactor("get", "nodes", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("get denied") })
	monitor.refreshAt(t.Context(), monitor.lastGood.Add(cpuCapacityGrace))
	if p := monitor.snapshot(t.Context()); p.HoldReason == "" {
		t.Fatal("target permission failure should hold", p)
	}
}

func TestSameEndpointPinnedCapacityIsDistinctAndExcludedFromLocal(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "internal", "app", "testdata", "native-target.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct{ Config map[string]json.RawMessage }
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	var placement map[string]any
	if err := json.Unmarshal(document.Config["ExecutionPlacement"], &placement); err != nil {
		t.Fatal(err)
	}
	placement["schema"] = "oberth.execution-placement/branch-v2"
	delete(placement, "preflight")
	normal := placement["normal"].(map[string]any)
	delete(normal, "trusted_intent")
	placement["branch_policy_baseline"] = strings.Repeat("b", 40)
	placement["graph_digest"] = strings.Repeat("c", 64)
	placement["fragment_lock_digest"] = strings.Repeat("d", 64)
	encoded, err := json.Marshal(placement)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := argojob.ParseExecutionPlacement(encoded)
	if err != nil {
		t.Fatal(err)
	}
	fixture := struct{ Config argojob.Config }{Config: argojob.Config{ExecutionPlacement: selected}}
	endpoint := "https://api.example.test"
	targets := []app.ExecutionTarget{{ID: "pinned", Config: fixture.Config, Transport: &rest.Config{Host: endpoint}}}
	excluded, err := pinnedSchedulingNodes(endpoint, targets)
	nodeName := fixture.Config.ExecutionPlacement.CPUNodeName()
	if err != nil || !slices.Equal(excluded, []string{nodeName}) {
		t.Fatal(excluded, err)
	}
	for _, tc := range []struct {
		name, cpu        string
		denied, cordoned bool
		want             int64
	}{
		{name: "healthy", cpu: "4", want: 3200},
		{name: "larger", cpu: "32", want: 25600},
		{name: "denied", cpu: "4", denied: true},
		{name: "cordoned", cpu: "4", cordoned: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pinned := cpuTestNode(nodeName, tc.cpu)
			pinned.Spec.Unschedulable = tc.cordoned
			client := fake.NewClientset(cpuTestNode("local-worker", "12"), pinned)
			if tc.denied {
				client.PrependReactor("get", "nodes", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("get denied") })
			}
			local := newCPUCapacity(t.Context(), client, endpoint, "", excluded, 20, 25).snapshot(t.Context())
			remote := newCPUCapacity(t.Context(), client, endpoint, nodeName, nil, 20, 25, fixture.Config.ExecutionPlacement.CPUTolerations()...).snapshot(t.Context())
			if local.Pool == remote.Pool || local.Milli != 9600 || local.HoldReason != "" {
				t.Fatal("target changed healthy local capacity", local, remote)
			}
			if remote.Milli != tc.want || (remote.HoldReason != "") != (tc.want == 0) {
				t.Fatal("incorrect pinned capacity", remote)
			}
		})
	}
	if a, b := schedulingPool(endpoint, "", []string{"b", "a", "a"}), schedulingPool(endpoint+"/", "", []string{"a", "b"}); a != b {
		t.Fatal("noncanonical eligible scope", a, b)
	}
	targets[0].Config.ExecutionPlacement = nil
	if _, err := pinnedSchedulingNodes(endpoint, targets); err == nil {
		t.Fatal("overlapping same-endpoint unpinned pool accepted")
	}
}
