package argowatch

import (
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

func TestNodeObserverUsesOnlyLinkedWorkflowsAndSemanticFacts(t *testing.T) {
	t.Parallel()
	pods := cache.NewSharedIndexInformer(&cache.ListWatch{}, &core.Pod{}, 0, cache.Indexers{"node": podNodeIndex})
	nodes := cache.NewSharedIndexInformer(&cache.ListWatch{}, &core.Node{}, 0, nil)
	h := &Hub{pods: pods, nodes: nodes, healthy: [3]bool{true, true, true}}
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "pod", Labels: map[string]string{"workflows.argoproj.io/workflow": "own"}}, Spec: core.PodSpec{NodeName: "assigned"}}
	if err := pods.GetIndexer().Add(pod); err != nil {
		t.Fatal(err)
	}
	var changed []string
	h.OnChange(func(workflow string) { changed = append(changed, workflow) })
	h.nodeChanged("unrelated")
	if len(changed) != 0 {
		t.Fatal("unrelated Node fanned out", changed)
	}
	h.nodeChanged("assigned")
	if len(changed) != 1 || changed[0] != "own" {
		t.Fatal("assigned Node missed its Pod's owner", changed)
	}
	before := &core.Node{ObjectMeta: meta.ObjectMeta{Name: "assigned", UID: "node-uid", ResourceVersion: "1"}, Status: core.NodeStatus{Conditions: []core.NodeCondition{{Type: core.NodeMemoryPressure, Status: core.ConditionTrue}}}}
	after := before.DeepCopy()
	after.ResourceVersion = "2"
	after.Status.Conditions[0].Message = "private native text"
	after.Status.Conditions[0].LastHeartbeatTime = meta.NewTime(time.Unix(99, 0))
	h.update(before, after)
	if len(changed) != 1 {
		t.Fatal("heartbeat enqueued an unchanged native observation", changed)
	}
	projected := compactNode(after)
	if projected.ResourceVersion != "" || !projected.Status.Conditions[0].LastHeartbeatTime.IsZero() {
		t.Fatal("Node cache retained heartbeat state")
	}
	after.Status.Conditions[0].Status = core.ConditionFalse
	h.update(before, after)
	if len(changed) < 2 {
		t.Fatal("cleared pressure failed to invalidate the linked run")
	}
}

func TestNativeResyncDoesNotInvalidateObserverReads(t *testing.T) {
	h := &Hub{}
	calls := 0
	h.OnChange(func(string) { calls++ })
	workflow := &wf.Workflow{ObjectMeta: meta.ObjectMeta{Name: "own", ResourceVersion: "1"}}
	changed := workflow.DeepCopy()
	changed.ResourceVersion = "2"
	h.update(workflow, changed)
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "pod", ResourceVersion: "1", Labels: map[string]string{"workflows.argoproj.io/workflow": "own"}}, Status: core.PodStatus{Conditions: []core.PodCondition{{Type: core.PodScheduled, Status: core.ConditionFalse}}}}
	next := pod.DeepCopy()
	next.ResourceVersion = "2"
	next.Status.Conditions[0].LastProbeTime = meta.NewTime(time.Unix(99, 0))
	h.update(pod, next)
	if calls != 0 {
		t.Fatal("unchanged resync caused native reads", calls)
	}
	next.Status.Conditions[0].Message = "new scheduler evidence"
	h.update(pod, next)
	if calls == 0 {
		t.Fatal("changed native evidence was suppressed")
	}
}
