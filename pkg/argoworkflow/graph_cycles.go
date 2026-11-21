package argoworkflow

import (
	"github.com/oberthci/oberth/internal/admissioncontract"
)

// Strong components include edges to already visited nodes: cached local
// recursion cannot hide a cycle that crosses a shared-reference boundary.
type components struct {
	next       int
	index, low map[*node]int
	active     map[*node]bool
	stack      []*node
	groups     [][]*node
}

func (g *graph) checkCycles() {
	s := components{index: map[*node]int{}, low: map[*node]int{}, active: map[*node]bool{}}
	for _, n := range g.nodes {
		if s.index[n] == 0 {
			s.visit(n)
		}
	}
	for _, group := range s.groups {
		members := map[*node]bool{}
		for _, n := range group {
			members[n] = true
		}
		for _, n := range group {
			for _, c := range edges(n) {
				if c.ref != nil && members[c.target] {
					g.refuse(c.path, "cycle")
				}
			}
		}
	}
}
func (s *components) visit(n *node) {
	s.next++
	s.index[n] = s.next
	s.low[n] = s.next
	s.stack = append(s.stack, n)
	s.active[n] = true
	for _, c := range edges(n) {
		if c.target == nil {
			continue
		}
		if s.index[c.target] == 0 {
			s.visit(c.target)
			s.low[n] = min(s.low[n], s.low[c.target])
		} else if s.active[c.target] {
			s.low[n] = min(s.low[n], s.index[c.target])
		}
	}
	if s.low[n] != s.index[n] {
		return
	}
	var group []*node
	for {
		last := s.stack[len(s.stack)-1]
		s.stack = s.stack[:len(s.stack)-1]
		s.active[last] = false
		group = append(group, last)
		if last == n {
			break
		}
	}
	s.groups = append(s.groups, group)
}
func edges(n *node) []*call {
	result := make([]*call, 0, len(n.calls))
	for _, c := range n.calls {
		if len(c.candidates) == 0 {
			result = append(result, c)
		}
		for _, n := range c.candidates {
			copy := *c
			copy.target = n
			copy.candidates = nil
			result = append(result, &copy)
		}
		result = append(result, c.hooks...)
	}
	return result
}
func (g *graph) checkDepth() {
	heights := map[*node]int{}
	for _, n := range g.nodes {
		if nodeHeight(n, heights, map[*node]bool{}) > admissioncontract.MaxNestingDepth {
			g.refuse(n.path, "depth")
		}
	}
}
func nodeHeight(n *node, heights map[*node]int, active map[*node]bool) int {
	if n == nil || active[n] {
		return 0
	}
	if h := heights[n]; h != 0 {
		return h
	}
	active[n] = true
	h := 1
	for _, c := range edges(n) {
		h = max(h, 1+nodeHeight(c.target, heights, active))
	}
	delete(active, n)
	heights[n] = h
	return h
}
