package argoworkflow

import "slices"

// executionOrder places DAG tasks by dependency level, retaining declaration
// order within each level. Steps calls retain their declared group order.
// Invalid cycles retain the remaining declaration order for inspection.
func executionOrder(calls []*call) ([]*call, map[*call]int) {
	tasks := map[string]bool{}
	for _, c := range calls {
		if c.dag {
			tasks[c.name] = true
		}
	}
	levels := map[*call]int{}
	if len(tasks) == 0 {
		return calls, levels
	}
	placed := map[string]bool{}
	rest, ordered := slices.Clone(calls), make([]*call, 0, len(calls))
	for level := 0; len(rest) > 0; level++ {
		ready, blocked := []*call{}, []*call{}
		for _, c := range rest {
			if slices.ContainsFunc(c.after, func(name string) bool { return tasks[name] && !placed[name] && name != c.name }) {
				blocked = append(blocked, c)
			} else {
				ready = append(ready, c)
			}
		}
		if len(ready) == 0 {
			return append(ordered, rest...), levels
		}
		for _, c := range ready {
			placed[c.name], levels[c] = true, level
		}
		ordered = append(ordered, ready...)
		rest = blocked
	}
	return ordered, levels
}
