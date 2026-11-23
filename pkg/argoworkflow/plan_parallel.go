package argoworkflow

import (
	"strconv"
	"strings"
)

// parallelCalls assigns a scope-local group to leaf siblings at the same DAG
// dependency level, or in the same steps group. Nested templates compute their
// own groups so serial descendants and exit hooks never share a parallel border.
func (p *planner) parallelCalls(ordered []*call) map[*call]int {
	levels := map[string]int{}
	keys := map[*call]string{}
	counts := map[string]int{}
	for _, c := range ordered {
		key, _, _ := strings.Cut(c.suffix, "]")
		if c.dag {
			level := 0
			for _, name := range c.after {
				if prior, ok := levels[name]; ok {
					level = max(level, prior+1)
				}
			}
			levels[c.name] = level
			key = strconv.Itoa(level)
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
