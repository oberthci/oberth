package argowatch

import (
	"reflect"
	"slices"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func compactNode(node *core.Node) *core.Node {
	conditions := slices.DeleteFunc(slices.Clone(node.Status.Conditions), func(c core.NodeCondition) bool {
		return c.Type != core.NodeReady && c.Type != core.NodeMemoryPressure && c.Type != core.NodeDiskPressure && c.Type != core.NodePIDPressure
	})
	conditions = conditions[:min(len(conditions), 4)]
	for i := range conditions {
		conditions[i].LastHeartbeatTime = meta.Time{}
		conditions[i].Message, conditions[i].Reason = "", ""
		if conditions[i].Status != core.ConditionTrue && conditions[i].Status != core.ConditionFalse {
			conditions[i].Status = core.ConditionUnknown
		}
	}
	return &core.Node{ObjectMeta: meta.ObjectMeta{Name: node.Name, UID: node.UID},
		Spec: core.NodeSpec{Taints: boundedTaints(node.Spec.Taints)}, Status: core.NodeStatus{Conditions: conditions}}
}

func podNodeIndex(obj any) ([]string, error) {
	if pod, ok := obj.(*core.Pod); ok && pod.Spec.NodeName != "" {
		return []string{pod.Spec.NodeName}, nil
	}
	return nil, nil
}

func (h *Hub) update(previous, current any) {
	if a, ok := previous.(*core.Node); ok {
		if b, ok := current.(*core.Node); ok {
			if !reflect.DeepEqual(compactNode(a), compactNode(b)) {
				h.change(current)
			}
			return
		}
	}
	if sameNativeObject(previous, current) {
		return
	}
	h.change(previous)
	h.change(current)
}

// Node invalidation follows reverse Pod links; an unrelated Node cannot wake
// the installation's other runs.
func (h *Hub) nodeChanged(name string) {
	objects, err := h.pods.GetIndexer().ByIndex("node", name)
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, object := range objects {
		pod := object.(*core.Pod)
		workflow := pod.Labels["workflows.argoproj.io/workflow"]
		if workflow == "" || seen[workflow] {
			continue
		}
		seen[workflow] = true
		h.mu.RLock()
		fn := h.changed
		h.mu.RUnlock()
		if fn != nil {
			fn(workflow)
		}
	}
}

func (h *Hub) NodesForPods(pods []*core.Pod) []*core.Node {
	h.mu.RLock()
	healthy := h.healthy[2]
	h.mu.RUnlock()
	if h.nodes == nil || !healthy {
		return nil
	}
	var out []*core.Node
	seen := map[string]bool{}
	for _, pod := range pods {
		name := pod.Spec.NodeName
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if object, found, err := h.nodes.GetIndexer().GetByKey(name); err == nil && found {
			out = append(out, object.(*core.Node).DeepCopy())
		}
	}
	return out
}

func boundedTaints(input []core.Taint) []core.Taint {
	out := make([]core.Taint, 0, min(len(input), 64))
	for _, taint := range input {
		if len(out) == 64 {
			break
		}
		if boundedTaint(taint) {
			out = append(out, taint)
		}
	}
	return out
}

func boundedTaint(taint core.Taint) bool {
	return len(taint.Key)+len(taint.Value) <= 512 && (taint.Effect == core.TaintEffectNoSchedule || taint.Effect == core.TaintEffectPreferNoSchedule || taint.Effect == core.TaintEffectNoExecute)
}

// Resource versions and probe heartbeats do not change native evidence. Ordered
// health and receipt input is delivered before this observer invalidation.
func sameNativeObject(previous, current any) bool {
	a, aok := previous.(runtime.Object)
	b, bok := current.(runtime.Object)
	if !aok || !bok {
		return false
	}
	return reflect.DeepEqual(nativeUpdateFacts(a), nativeUpdateFacts(b))
}

func nativeUpdateFacts(object runtime.Object) runtime.Object {
	object = object.DeepCopyObject()
	if m, ok := object.(meta.Object); ok {
		m.SetResourceVersion("")
		m.SetManagedFields(nil)
	}
	if pod, ok := object.(*core.Pod); ok {
		for i := range pod.Status.Conditions {
			pod.Status.Conditions[i].LastProbeTime = meta.Time{}
		}
	}
	return object
}
