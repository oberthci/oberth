package argoworkflow

import (
	"strconv"
	"strings"
)

// parallelCalls assigns a scope-local group to leaf siblings at the same DAG
// dependency level, or in the same steps group. Nested templates compute their
// own groups so serial descendants and exit hooks never share a parallel border.
func (p *planner) parallelCalls(ordered []*call, levels map[*call]int) map[*call]int {
	keys := map[*call]string{}
	counts := map[string]int{}
	for _, c := range ordered {
		key, _, _ := strings.Cut(c.suffix, "]")
		if c.dag {
			key = strconv.Itoa(levels[c])
		}
		if parallelLeaf(c) {
			keys[c] = key
			counts[key]++
		}
	}
	groups := map[string]int{}
	out := map[*call]int{}
	for _, c := range ordered {
		key, ok := keys[c]
		if !ok || counts[key] < 2 {
			continue
		}
		if groups[key] == 0 {
			p.nextGroup++
			groups[key] = p.nextGroup
		}
		out[c] = groups[key]
	}
	return out
}

func parallelLeaf(c *call) bool {
	return c.target != nil && leaf(c.target.template)
}

func (p *planner) fanOutGroup(c *call, group int, dynamic bool) int {
	if group == 0 && parallelLeaf(c) && (dynamic || len(c.items) > 1) {
		p.nextGroup++
		return p.nextGroup
	}
	return group
}

func (p *planner) collapsedCall(c *call, path string, group int) bool {
	entry := p.dynamicEntry(path, c.name, c.target, group)
	entry.FanOut = parallelLeaf(c) && (c.dynamic || len(c.items) > 0)
	return p.append(entry)
}
