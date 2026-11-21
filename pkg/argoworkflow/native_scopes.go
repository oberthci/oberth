package argoworkflow

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
)

// Native scopes are connected declarations in one file. Diagnostics use copies;
// calls into retained files are resolved by Argo, never by a substitute validator.
func nativeScopes(g *graph) [][]*node {
	adjacent := nativeAdjacency(g)
	seen := map[*node]bool{}
	var groups [][]*node
	for _, root := range g.nodes {
		if seen[root] {
			continue
		}
		group := []*node{}
		pending := []*node{root}
		seen[root] = true
		for len(pending) > 0 {
			n := pending[len(pending)-1]
			pending = pending[:len(pending)-1]
			group = append(group, n)
			for _, next := range adjacent[n] {
				if !seen[next] {
					seen[next] = true
					pending = append(pending, next)
				}
			}
		}
		groups = append(groups, group)
	}
	return groups
}

func nativeAdjacency(g *graph) map[*node][]*node {
	adjacent := map[*node][]*node{}
	for _, n := range g.nodes {
		for _, c := range edges(n) {
			if c.target != nil && c.target.scope == n.scope {
				adjacent[n] = append(adjacent[n], c.target)
				adjacent[c.target] = append(adjacent[c.target], n)
			}
		}
	}
	if len(g.roots) > 0 && g.roots[0].target != nil {
		root := g.roots[0].target
		for _, c := range g.roots[1:] {
			if c.target != nil && c.target.scope == root.scope {
				adjacent[root] = append(adjacent[root], c.target)
				adjacent[c.target] = append(adjacent[c.target], root)
			}
		}
	}
	return adjacent
}

func nativeFindings(ctx context.Context, wf *wfv1.Workflow, objects retainedTemplates, g *graph) error {
	groups := nativeScopes(g)
	if !nativeWorkAllowed(groups, wf) {
		return refusal("native_schema", "native_work")
	}
	// Release and unknown-kind inspections allow unresolved release facts;
	// a known build must declare its own parameters.
	if g.offline() && (g.policy.Release || g.unknownRole()) {
		wf = releaseSchemaParameters(wf)
	}
	c := nativeCollector{diagnostics: indexNativeCalls(g), seen: map[admissioncontract.Finding]bool{}}
	for _, group := range groups {
		if c.stopped {
			break
		}
		candidate, _ := nativeScopeWorkflow(wf, group)
		if g.offline() {
			maskMissingReferences(candidate, group)
		}
		if err := c.scope(ctx, candidate, objects, group); err != nil {
			return err
		}
	}
	if len(c.result.Findings) == 0 {
		return nil
	}
	g.sortFindings(c.result.Findings)
	details := strings.Split(strings.TrimSpace(c.result.Detail), "\n")
	slices.Sort(details)
	c.result.Detail = strings.Join(details, "\n")
	return &c.result
}

func maskMissingReferences(candidate *wfv1.Workflow, group []*node) {
	for _, n := range group {
		for _, c := range edges(n) {
			if c.ref != nil && c.target == nil {
				muteNativeTemplate(candidate, n.path, group)
				break
			}
		}
	}
}

func nativeScopeWorkflow(wf *wfv1.Workflow, group []*node) (*wfv1.Workflow, bool) {
	candidate := *wf
	candidate.Spec = wf.Spec
	candidate.Status = wfv1.WorkflowStatus{}
	candidate.Spec.Templates = nil
	candidate = *candidate.DeepCopy()
	names := map[string]bool{}
	for _, n := range group {
		if n.inlineDepth == 0 {
			candidate.Spec.Templates = append(candidate.Spec.Templates, *n.template.DeepCopy())
			names[n.template.Name] = true
		}
	}
	ignore := !names[wf.Spec.Entrypoint] || group[0].scope != ""
	if ignore {
		candidate.Spec.OnExit = ""
		candidate.Spec.Hooks = nil
		candidate.Spec.Entrypoint = ""
	}
	return &candidate, ignore
}

func nativeWorkAllowed(groups [][]*node, wf *wfv1.Workflow) bool {
	work := 0
	base := *wf
	base.Spec = wf.Spec
	base.Spec.Templates = nil
	raw, _ := json.Marshal(base)
	globalCost := max(len(wf.Spec.Arguments.Parameters)+len(wf.Labels)+len(wf.Annotations), (len(raw)+1023)/1024)
	for _, group := range groups {
		pending := append([]*node(nil), group...)
		seen := map[*node]bool{}
		work += globalCost
		for len(pending) > 0 {
			n := pending[len(pending)-1]
			pending = pending[:len(pending)-1]
			if seen[n] {
				continue
			}
			seen[n] = true
			raw, _ := json.Marshal(n.template)
			work += max(1, len(containers(n.template)), (len(raw)+1023)/1024)
			for _, c := range edges(n) {
				work++
				if c.target != nil {
					pending = append(pending, c.target)
				}
			}
			if work > maxInspectionNodes {
				return false
			}
		}
	}
	return true
}

type nativeDiagnostics struct {
	targets map[string]*node
	callers map[string][]string
}

func indexNativeCalls(g *graph) nativeDiagnostics {
	d := nativeDiagnostics{targets: map[string]*node{}, callers: map[string][]string{}}
	for _, n := range g.nodes {
		for _, c := range edges(n) {
			if c.target != nil {
				d.targets[c.path] = c.target
				d.callers[c.target.path] = append(d.callers[c.target.path], c.path)
			}
		}
	}
	return d
}
func (d nativeDiagnostics) location(scope, detail string) string {
	location := nativeErrorPath(detail)
	if scope != "" {
		location = "templates[" + scope + "]." + strings.TrimPrefix(location, "spec.")
	}
	if target := d.targets[location]; target != nil {
		return target.path
	}
	return location
}
func (d nativeDiagnostics) callersFor(location string) string {
	for location != "" {
		if callers := d.callers[location]; len(callers) > 0 {
			return "; called from " + strings.Join(callers, ", ")
		}
		at := strings.LastIndex(location, ".")
		if at < 0 {
			break
		}
		location = location[:at]
	}
	return ""
}
