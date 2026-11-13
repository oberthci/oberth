package argoworkflow

import wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

// Cache immutable graph edges, never context-dependent traversal results.
func (walk *planWalk) callsFor(template *wfv1.Template) []invocation {
	if calls, ok := walk.calls[template]; ok {
		return calls
	}
	calls := invocations(template)
	walk.calls[template] = calls
	return calls
}

func (walk *planWalk) reachableTarget(called invocation) (*wfv1.Template, bool) {
	if called.expands {
		return nil, false
	}
	if called.template != "" {
		if _, cut := walk.onPath[called.template]; cut {
			return nil, true
		}
	}
	return walk.resolve(called), false
}

// A subtree unable to reach a Pod in this recursion context cannot add rows.
// Pruning it avoids enumerating exponentially many paths through empty cycles.
// The reachability search visits each graph vertex once and stores no result.
func (walk *planWalk) reachesPod(start *wfv1.Template) (bool, bool) {
	pending := []*wfv1.Template{start}
	seen := map[*wfv1.Template]bool{start: true}
	cut := false
	for i := 0; i < len(pending); i++ {
		template := pending[i]
		composite := template.DAG != nil || len(template.Steps) != 0
		if !composite && podProducing(template) {
			return true, cut
		}
		for _, called := range walk.callsFor(template) {
			target, backEdge := walk.reachableTarget(called)
			cut = cut || backEdge
			if target != nil && !seen[target] {
				seen[target] = true
				pending = append(pending, target)
			}
		}
	}
	// A cycle wholly inside the pruned region must also be recorded. Repeated
	// vertices alone are insufficient: an acyclic diamond also reconverges.
	return false, cut || walk.containsCycle(pending, seen)
}

// Kahn's algorithm detects cycles without another recursive walk.
func (walk *planWalk) containsCycle(nodes []*wfv1.Template, seen map[*wfv1.Template]bool) bool {
	incoming := make(map[*wfv1.Template]int, len(nodes))
	for _, node := range nodes {
		for _, called := range walk.callsFor(node) {
			target, _ := walk.reachableTarget(called)
			if target != nil && seen[target] {
				incoming[target]++
			}
		}
	}
	var ready []*wfv1.Template
	for _, node := range nodes {
		if incoming[node] == 0 {
			ready = append(ready, node)
		}
	}
	for i := 0; i < len(ready); i++ {
		for _, called := range walk.callsFor(ready[i]) {
			target, _ := walk.reachableTarget(called)
			if target == nil || !seen[target] {
				continue
			}
			incoming[target]--
			if incoming[target] == 0 {
				ready = append(ready, target)
			}
		}
	}
	return len(ready) != len(nodes)
}
