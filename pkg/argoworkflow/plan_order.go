package argoworkflow

import "slices"

// executionOrder lists a template's calls in the order they run. Steps calls are
// already in group order (parallel steps keep declaration order). DAG tasks are
// put in a stable topological order: a task follows every task of the same DAG it
// depends on, and ties keep declaration order. A dependency cycle, which Argo
// refuses, leaves the remaining tasks in declaration order.
func executionOrder(calls []*call) []*call {
	tasks := map[string]bool{}
	for _, c := range calls {
		if c.dag {
			tasks[c.name] = true
		}
	}
	if len(tasks) == 0 {
		return calls
	}
	placed := map[string]bool{}
	ready := func(c *call) bool {
		return !slices.ContainsFunc(c.after, func(name string) bool { return tasks[name] && !placed[name] && name != c.name })
	}
	rest, ordered := slices.Clone(calls), make([]*call, 0, len(calls))
	for len(rest) > 0 {
		next := slices.IndexFunc(rest, ready)
		if next < 0 {
			return append(ordered, rest...)
		}
		placed[rest[next].name] = true
		ordered = append(ordered, rest[next])
		rest = slices.Delete(rest, next, next+1)
	}
	return ordered
}
