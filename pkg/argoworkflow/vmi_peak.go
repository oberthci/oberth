package argoworkflow

import (
	"regexp"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

// Only unconditional, bounded graphs with successful dependency barriers earn
// concurrency credits. A create task finishing is NOT the end of a VM lifetime.
const maxVMIGraphNodes = 256

var successfulDependency = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*(\.Succeeded)?$`)

type vmiGraphEvent struct {
	node                   int
	template, name, action string
	demand                 VMIDemand
}
type vmiGraph struct {
	edges       [][]int
	events      []vmiGraphEvent
	templates   map[string]wfv1.Template
	stack       map[string]bool
	cleanupSeen map[string]bool
}

func (g *vmiGraph) node() (int, bool) {
	if len(g.edges) >= maxVMIGraphNodes {
		return 0, false
	}
	n := len(g.edges)
	g.edges = append(g.edges, nil)
	return n, true
}
func (g *vmiGraph) edge(a, b int) { g.edges[a] = append(g.edges[a], b) }

func (g *vmiGraph) expand(name string) (int, int, bool) {
	t, ok := g.templates[name]
	if !ok || g.stack[name] || t.RetryStrategy != nil || t.Daemon != nil {
		return 0, 0, false
	}
	g.stack[name] = true
	defer delete(g.stack, name)
	start, ok := g.node()
	if !ok {
		return 0, 0, false
	}
	if t.DAG == nil && len(t.Steps) == 0 {
		if t.Resource != nil {
			operation, err := VMIOperation(t.Resource)
			if err != nil {
				return 0, 0, false
			}
			g.events = append(g.events, vmiGraphEvent{node: start, template: name, name: operation.Name, action: operation.Action, demand: operation.Demand})
		}
		return start, start, true
	}
	end, ok := g.node()
	if !ok {
		return 0, 0, false
	}
	if t.DAG != nil {
		if len(t.Steps) != 0 || t.DAG.Target != "" || len(t.DAG.Tasks) == 0 {
			return 0, 0, false
		}
		starts, ends := map[string]int{}, map[string]int{}
		deps := map[string][]string{}
		hasDepends, hasLegacy := false, false
		for _, task := range t.DAG.Tasks {
			if !vmiDNSLabelPattern.MatchString(task.Name) || task.Template == "" || task.TemplateRef != nil || task.Inline != nil || task.When != "" || task.OnExit != "" || task.ContinueOn != nil || len(task.WithItems) != 0 || task.WithParam != "" || task.WithSequence != nil || len(task.Hooks) != 0 {
				return 0, 0, false
			}
			if _, duplicate := starts[task.Name]; duplicate {
				return 0, 0, false
			}
			ds := task.Dependencies
			hasDepends = hasDepends || task.Depends != ""
			hasLegacy = hasLegacy || len(ds) > 0
			if hasDepends && hasLegacy {
				return 0, 0, false
			}
			if task.Depends != "" {
				if len(ds) != 0 {
					return 0, 0, false
				}
				for _, part := range strings.Split(task.Depends, "&&") {
					part = strings.TrimSpace(part)
					if !successfulDependency.MatchString(part) {
						return 0, 0, false
					}
					ds = append(ds, strings.TrimSuffix(part, ".Succeeded"))
				}
			}
			a, b, ok := g.expand(task.Template)
			if !ok {
				return 0, 0, false
			}
			starts[task.Name], ends[task.Name], deps[task.Name] = a, b, ds
		}
		for name, a := range starts {
			if len(deps[name]) == 0 {
				g.edge(start, a)
			}
			for _, dep := range deps[name] {
				b, ok := ends[dep]
				if !ok || dep == name {
					return 0, 0, false
				}
				g.edge(b, a)
			}
			g.edge(ends[name], end)
		}
	} else {
		previous := []int{start}
		stepNames := map[string]bool{}
		for _, group := range t.Steps {
			if len(group.Steps) == 0 {
				return 0, 0, false
			}
			next := []int{}
			for _, step := range group.Steps {
				if !vmiDNSLabelPattern.MatchString(step.Name) || stepNames[step.Name] || step.Template == "" || step.TemplateRef != nil || step.Inline != nil || step.When != "" || step.OnExit != "" || step.ContinueOn != nil || len(step.WithItems) != 0 || step.WithParam != "" || step.WithSequence != nil || len(step.Hooks) != 0 {
					return 0, 0, false
				}
				stepNames[step.Name] = true
				a, b, ok := g.expand(step.Template)
				if !ok {
					return 0, 0, false
				}
				for _, p := range previous {
					g.edge(p, a)
				}
				next = append(next, b)
			}
			previous = next
		}
		for _, p := range previous {
			g.edge(p, end)
		}
	}
	return start, end, true
}

// Exit cleanup is deliberately excluded from lifetime barriers. A create in an
// exit handler (or an unrecognized exit graph) disables the optimization.
func (g *vmiGraph) exitOnlyCleanup(name string, stack map[string]bool) bool {
	if g.cleanupSeen[name] {
		return true
	}
	t, ok := g.templates[name]
	if !ok || stack[name] {
		return false
	}
	stack[name] = true
	defer delete(stack, name)
	if t.Resource != nil && t.Resource.Action != "delete" && t.Resource.Action != "get" {
		return false
	}
	if t.DAG != nil {
		for _, task := range t.DAG.Tasks {
			if task.OnExit != "" || len(task.Hooks) != 0 || task.TemplateRef != nil || task.Inline != nil || !g.exitOnlyCleanup(task.Template, stack) {
				return false
			}
		}
	}
	for _, group := range t.Steps {
		for _, step := range group.Steps {
			if step.OnExit != "" || len(step.Hooks) != 0 || step.TemplateRef != nil || step.Inline != nil || !g.exitOnlyCleanup(step.Template, stack) {
				return false
			}
		}
	}
	g.cleanupSeen[name] = true
	return true
}

func peakVMIDemand(wf *wfv1.Workflow) (VMIDemand, bool) {
	if wf.Spec.TemplateDefaults != nil || wf.Spec.RetryStrategy != nil || len(wf.Spec.Hooks) != 0 {
		return VMIDemand{}, false
	}
	g := vmiGraph{templates: map[string]wfv1.Template{}, stack: map[string]bool{}, cleanupSeen: map[string]bool{}}
	for _, t := range wf.Spec.Templates {
		if _, exists := g.templates[t.Name]; exists {
			return VMIDemand{}, false
		}
		g.templates[t.Name] = t
	}
	if wf.Spec.OnExit != "" && !g.exitOnlyCleanup(wf.Spec.OnExit, map[string]bool{}) {
		return VMIDemand{}, false
	}
	if _, _, ok := g.expand(wf.Spec.Entrypoint); !ok {
		return VMIDemand{}, false
	}
	n := len(g.edges)
	reach := make([][]bool, n)
	for i := range reach {
		reach[i] = make([]bool, n)
		for _, j := range g.edges[i] {
			reach[i][j] = true
		}
	}
	for k := range n {
		for i := range n {
			if reach[i][k] {
				for j := range n {
					reach[i][j] = reach[i][j] || reach[k][j]
				}
			}
		}
	}
	for i := range n {
		if reach[i][i] {
			return VMIDemand{}, false
		}
	}
	creates := []vmiGraphEvent{}
	seen := map[string]bool{}
	for _, e := range g.events {
		if e.action == "create" {
			creates = append(creates, e)
			seen[e.template] = true
		}
	}
	// Preserve conservative accounting for declared creates not understood by the
	// reachable entrypoint graph (including old/non-DAG authored workflows).
	for _, t := range wf.Spec.Templates {
		if t.Resource != nil && t.Resource.Action == "create" && !seen[t.Name] {
			return VMIDemand{}, false
		}
	}
	before := make([][]bool, len(creates))
	for i, a := range creates {
		before[i] = make([]bool, len(creates))
		for j, b := range creates {
			if i == j {
				continue
			}
			for _, d := range g.events {
				if d.action == "delete" && d.name == a.name && reach[a.node][d.node] && reach[d.node][b.node] {
					before[i][j] = true
					break
				}
			}
		}
	}
	vms, cores, memory := make([]int64, len(creates)), make([]int64, len(creates)), make([]int64, len(creates))
	for i, c := range creates {
		vms[i], cores[i], memory[i] = 1, int64(c.demand.Cores), c.demand.MemoryBytes
	}
	return VMIDemand{VMs: int(maxWeightedVMAntichain(before, vms)), Cores: int(maxWeightedVMAntichain(before, cores)), MemoryBytes: maxWeightedVMAntichain(before, memory)}, true
}

// Weighted Dilworth: replace each weighted vertex by incomparable unit copies.
// A capacitated bipartite matching computes their chain cover without expanding
// bytes of memory into individual vertices. Max antichain = total - max flow.
func maxWeightedVMAntichain(before [][]bool, weights []int64) int64 {
	n := len(weights)
	source, sink := 2*n, 2*n+1
	type edge struct {
		to, reverse int
		capacity    int64
	}
	graph := make([][]edge, 2*n+2)
	add := func(a, b int, c int64) {
		graph[a] = append(graph[a], edge{b, len(graph[b]), c})
		graph[b] = append(graph[b], edge{a, len(graph[a]) - 1, 0})
	}
	var total int64
	for _, w := range weights {
		total += w
	}
	for i, w := range weights {
		add(source, i, w)
		add(n+i, sink, w)
		for j := range weights {
			if before[i][j] {
				add(i, n+j, total+1)
			}
		}
	}
	var flow int64
	for {
		level := make([]int, len(graph))
		for i := range level {
			level[i] = -1
		}
		level[source] = 0
		queue := []int{source}
		for len(queue) > 0 {
			a := queue[0]
			queue = queue[1:]
			for _, e := range graph[a] {
				if e.capacity > 0 && level[e.to] < 0 {
					level[e.to] = level[a] + 1
					queue = append(queue, e.to)
				}
			}
		}
		if level[sink] < 0 {
			return total - flow
		}
		next := make([]int, len(graph))
		var send func(int, int64) int64
		send = func(a int, limit int64) int64 {
			if a == sink {
				return limit
			}
			for next[a] < len(graph[a]) {
				i := next[a]
				e := graph[a][i]
				if e.capacity > 0 && level[e.to] == level[a]+1 {
					sent := send(e.to, min(limit, e.capacity))
					if sent > 0 {
						graph[a][i].capacity -= sent
						graph[e.to][e.reverse].capacity += sent
						return sent
					}
				}
				next[a]++
			}
			return 0
		}
		for {
			sent := send(source, total)
			if sent == 0 {
				break
			}
			flow += sent
		}
	}
}
