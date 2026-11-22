package argoworkflow

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
)

type planner struct {
	credentials      map[*node]bool
	omitted          map[*call]bool
	emptyBytes       int
	plan             admissioncontract.Plan
	visits, bytes    int
	active           map[*node]bool
	credentialIntent bool
}

func (g *graph) plan() admissioncontract.Plan { return g.planWithOmissions(nil) }

func (g *graph) planWithOmissions(omitted map[*call]bool) admissioncontract.Plan {
	p := planner{credentials: map[*node]bool{}, omitted: omitted, plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{}, Exact: true}, active: map[*node]bool{}, emptyBytes: emptyPlanBytes()}
	for _, n := range g.nodes {
		p.credentials[n] = g.nodeUsesCredentials(n)
		p.credentialIntent = p.credentialIntent || p.credentials[n]
	}
	for _, c := range g.roots {
		if !p.invoke(c, "") {
			p.plan.Entries = nil
			for _, root := range g.roots {
				p.omit(root)
			}
			p.bytes = 0
			p.placeholder("", g.workflow.Spec.Entrypoint, g.roots[0].target)
			break
		}
	}
	return p.plan
}
func (p *planner) invoke(c *call, parent string) bool {
	if c.inspectionOnly {
		return true
	}
	base := invocationPath(parent, c.suffix)
	start, size := len(p.plan.Entries), p.bytes
	if c.dynamic || dynamicItems(c.items) {
		p.omit(c)
		return p.placeholder(base, c.name, c.target)
	}
	success := true
	if len(c.items) > 0 {
		for i, item := range c.items {
			if !p.one(c, loopPath(base, i, item)) {
				success = false
				break
			}
		}
	} else {
		success = p.one(c, base)
	}
	if success && p.groupHooks(c, base) {
		return true
	}
	p.plan.Entries = p.plan.Entries[:start]
	p.bytes = size
	p.omit(c)
	return p.placeholder(base, c.name, c.target)
}
func (p *planner) one(c *call, path string) bool {
	if c.target == nil || p.active[c.target] {
		return p.placeholder(path, c.name, c.target)
	}
	if !p.visit(c.target, path, c.name) {
		return false
	}
	for _, hook := range c.hooks {
		if c.dag && hook.suffix != "onExit" {
			continue
		}
		if !p.invoke(hook, path) {
			return false
		}
	}
	return true
}
func (p *planner) groupHooks(c *call, base string) bool {
	if !c.dag {
		return true
	}
	for _, h := range c.hooks {
		if h.suffix == "onExit" && len(c.items) == 0 {
			continue
		}
		if !p.invoke(h, base) {
			return false
		}
	}
	return true
}
func dynamicItems(items []wfv1.Item) bool {
	for _, item := range items {
		if strings.Contains(string(item.Value), "{{") {
			return true
		}
	}
	return false
}
func (p *planner) visit(n *node, path, name string) bool {
	if p.visits >= admissioncontract.MaxPlannerVisits {
		return false
	}
	p.visits++
	if leaf(n.template) {
		if name == "" {
			name = n.template.Name
		}
		return p.append(admissioncontract.PlanEntry{Path: path, Name: name, Template: n.key, CredentialIntent: p.credentials[n]})
	}
	start, size := len(p.plan.Entries), p.bytes
	p.active[n] = true
	defer delete(p.active, n)
	for _, c := range executionOrder(n.calls) {
		if !p.invoke(c, path) {
			p.plan.Entries = p.plan.Entries[:start]
			p.bytes = size
			return p.placeholder(path, name, n)
		}
	}
	return true
}
func (p *planner) placeholder(path, name string, n *node) bool {
	p.plan.Exact = false
	template := ""
	if n != nil {
		template = n.key
		if name == "" {
			name = n.template.Name
		}
	}
	return p.append(admissioncontract.PlanEntry{Path: path, Name: name, Template: template, Dynamic: true, CredentialIntent: p.credentialIntent})
}
func (p *planner) append(entry admissioncontract.PlanEntry) bool {
	raw, err := json.Marshal(entry)
	if err != nil {
		return false
	}
	extra := len(raw)
	if len(p.plan.Entries) > 0 {
		extra++
	}
	overhead := p.emptyBytes
	if p.plan.Exact {
		overhead--
	}
	if len(p.plan.Entries) >= admissioncontract.MaxPlanEntries || p.bytes+extra+overhead > admissioncontract.MaxPlanBytes {
		return false
	}
	p.plan.Entries = append(p.plan.Entries, entry)
	p.bytes += extra
	return true
}
func emptyPlanBytes() int {
	raw, _ := json.Marshal(admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{}, Exact: false})
	return len(raw)
}
func invocationPath(parent, suffix string) string {
	if parent == "" || suffix == "" {
		return parent + suffix
	}
	if strings.HasPrefix(suffix, "[") {
		return parent + suffix
	}
	return parent + "." + suffix
}
func loopPath(base string, index int, item wfv1.Item) string {
	var description any = item
	switch item.GetType() {
	case wfv1.Map:
		vals := []string{}
		for key, value := range item.GetMapVal() {
			vals = append(vals, fmt.Sprintf("%s:%v", key, value))
		}
		slices.Sort(vals)
		description = strings.Join(vals, ",")
	case wfv1.List:
		description = item.GetListVal()
	}
	clean := strings.NewReplacer("(", "", ")", "").Replace(fmt.Sprint(description))
	return fmt.Sprintf("%s(%d:%s)", base, index, clean)
}

func (p *planner) omit(c *call) {
	if p.omitted != nil {
		p.omitted[c] = true
	}
}
