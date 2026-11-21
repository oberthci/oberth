package argojob

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"slices"
	"strings"
)

type retryPrefix struct {
	parent   string
	position int
}
type nativePath struct {
	name  string
	order []int
}
type nativePaths map[string]nativePath

func indexPaths(w *wf.Workflow) nativePaths {
	children := map[string]retryPrefix{}
	for _, n := range w.Status.Nodes {
		if n.Type == wf.NodeTypeRetry {
			for i, id := range n.Children {
				if c, ok := w.Status.Nodes[id]; ok && strings.HasPrefix(c.Name, n.Name+"(") {
					children[c.Name] = retryPrefix{n.Name, i}
				}
			}
		}
	}
	cache := map[string]nativePath{}
	paths := nativePaths{}
	for id, n := range w.Status.Nodes {
		p := resolveNativeName(n.Name, children, cache)
		p.name = strings.TrimPrefix(strings.TrimPrefix(p.name, w.Name), ".")
		paths[id] = p
	}
	return paths
}
func resolveNativeName(name string, children map[string]retryPrefix, cache map[string]nativePath) nativePath {
	if p, ok := cache[name]; ok {
		return p
	}
	if c, ok := children[name]; ok {
		p := resolveNativeName(c.parent, children, cache)
		p.order = append(append([]int{}, p.order...), c.position)
		cache[name] = p
		return p
	}
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] != '.' && name[i] != '[' {
			continue
		}
		prefix := name[:i]
		if _, ok := children[prefix]; ok {
			p := resolveNativeName(prefix, children, cache)
			p.name += name[i:]
			cache[name] = p
			return p
		}
	}
	p := nativePath{name: name}
	cache[name] = p
	return p
}
func (paths nativePaths) less(a, b wf.NodeStatus) bool {
	x, y := paths[a.ID].order, paths[b.ID].order
	if c := slices.Compare(x, y); c != 0 {
		return c < 0
	}
	if !a.StartedAt.Equal(&b.StartedAt) {
		return a.StartedAt.Before(&b.StartedAt)
	}
	return a.Name < b.Name
}
func nodePath(w *wf.Workflow, n wf.NodeStatus) string { return indexPaths(w)[n.ID].name }
