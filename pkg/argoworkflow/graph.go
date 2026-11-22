package argoworkflow

import (
	"context"
	"encoding/json"
	"fmt"
	"k8s.io/apimachinery/pkg/types"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/pkg/argoworkflow/defaults"
)

type node struct {
	isDefault        bool
	key, scope, path string
	template         *wfv1.Template
	calls            []*call
	inlineDepth      int
	referenceDepth   int
}
type call struct {
	readFailed         bool
	inspectionOnly     bool
	dag                bool
	candidates         []*node
	suffix, name, path string
	template           string
	ref                *wfv1.TemplateRef
	inline             *wfv1.Template
	arguments          wfv1.Arguments
	items              []wfv1.Item
	dynamic            bool
	target             *node
	hooks              []*call
	after              []string
}

const maxInspectionNodes = 65536

type templateRead struct {
	object *wfv1.WorkflowTemplate
	err    error
}
type templateScope struct {
	depth int
	nodes []*node
}
type graph struct {
	offlineValidation bool
	loginAudience     string
	secretGrants      map[string][]admissioncontract.GrantEvidence
	holdPlan          admissioncontract.Plan
	bindings          []sourcecontract.Binding
	stopped           bool
	constructions     int
	attempts          map[string]templateRead
	details           []string
	inventory         *sourceInventory
	scopes            map[string]*templateScope
	ctx               context.Context
	policy            Policy
	request           admissioncontract.PrepareRequest
	workflow          *wfv1.Workflow
	nodes             []*node
	byKey             map[string]*node
	objects           map[string]*wfv1.WorkflowTemplate
	snapshots         []admissioncontract.TemplateSnapshot
	roots             []*call
	findings          []admissioncontract.Finding
	reads             []error
}

func newGraph(ctx context.Context, wf *wfv1.Workflow, r admissioncontract.PrepareRequest, p Policy) *graph {
	return &graph{ctx: ctx, policy: p, request: r, workflow: wf, byKey: map[string]*node{}, objects: map[string]*wfv1.WorkflowTemplate{}, scopes: map[string]*templateScope{"": {}}, attempts: map[string]templateRead{}}
}
func (g *graph) offline() bool { return g.request.Mode == admissioncontract.ModeInspect }
func (g *graph) refuse(path, code string) {
	f := finding(path, code)
	f.Installation = code == "live"
	if f.Installation {
		repo := g.request.Repository
		if repo == "" {
			repo = "<repo>"
		}
		f.Command = publicerr.CheckCommand(repo, g.commandFile())
	}
	g.findings = append(g.findings, f)
}
func (g *graph) build() {
	if !g.continuing() {
		return
	}
	g.addScope("", g.workflow.Spec, "spec")
	if !g.constructing("spec.entrypoint") {
		return
	}
	g.roots = []*call{{template: g.workflow.Spec.Entrypoint, path: "spec.entrypoint"}}
	g.roots = append(g.roots, g.selectedHooks(g.workflow.Spec.Hooks, g.workflow.Spec.GetExitHook(g.workflow.Spec.Arguments), g.workflow.Spec.OnExit, "spec")...)
	for _, c := range g.roots {
		g.resolve(nil, c)
	}
	for i := 0; i < len(g.nodes) && !g.stopped; i++ {
		n := g.nodes[i]
		n.calls = g.templateCalls(n.template, n.path)
		for _, c := range n.calls {
			g.resolve(n, c)
		}
	}
	if g.stopped {
		return
	}
	g.checkCycles()
	g.checkDepth()
}
func (g *graph) addScope(scope string, spec wfv1.WorkflowSpec, where string) {
	for i := range spec.Templates {
		if !g.constructing(where) {
			return
		}
		t, err := defaults.Template(g.workflow.Spec.TemplateDefaults, &spec.Templates[i])
		p := fmt.Sprintf("%s.templates[%q]", where, spec.Templates[i].Name)
		if err != nil {
			g.refuse(p, "native")
			continue
		}
		key := templateKey(scope, t.Name)
		if g.byKey[key] != nil {
			g.refuse(p, "native")
			continue
		}
		n := &node{key: key, scope: scope, path: p, template: t, referenceDepth: g.scopes[scope].depth}
		g.byKey[key] = n
		g.nodes = append(g.nodes, n)
		g.scopes[scope].nodes = append(g.scopes[scope].nodes, n)
	}
}
func (g *graph) resolve(parent *node, c *call) {
	if !g.continuing() {
		return
	}
	scope := ""
	if parent != nil {
		scope = parent.scope
	}
	switch invocationForm(c) {
	case "inline":
		g.inline(parent, c)
	case "reference":
		g.resolveReference(parent, c)
	default:
		if dynamicLocal(parent, c) {
			g.dynamicTargets(c, scope)
		} else {
			c.target = g.byKey[templateKey(scope, c.template)]
		}
	}
	if g.stopped {
		return
	}
	g.unresolvedCall(c, scope)
	for _, h := range c.hooks {
		g.resolve(parent, h)
	}
}
func (g *graph) unresolvedCall(c *call, scope string) {
	if c.target == nil && !c.readFailed && len(c.candidates) == 0 && (!g.offline() || c.ref == nil) {
		if c.ref == nil && c.inline == nil {
			g.missingLocal(c, scope)
		} else {
			g.refuse(c.path, "reference")
		}
	}
}
func dynamicLocal(parent *node, c *call) bool {
	return parent != nil && strings.Contains(c.template, "{{")
}
func (g *graph) dynamicTargets(c *call, scope string) {
	c.dynamic = true
	for _, n := range g.scopes[scope].nodes {
		if !g.constructing(c.path) {
			return
		}
		c.candidates = append(c.candidates, n)
	}
}

func (g *graph) resolveReference(parent *node, c *call) {
	c.readFailed = true
	depth := 1
	if parent != nil {
		depth = parent.referenceDepth + 1
	}
	if depth > admissioncontract.MaxNestingDepth {
		g.refuse(c.path, "depth")
		return
	}
	if c.ref.ClusterScope || !literal(c.ref.Name) || !literal(c.ref.Template) {
		g.refuse(c.path, "reference")
		return
	}
	g.load(c.ref.Name, c.path, depth)
	c.target = g.byKey[templateKey(c.ref.Name, c.ref.Template)]
	c.readFailed = g.objects[c.ref.Name] == nil
}
func (g *graph) inline(parent *node, c *call) {
	if !g.constructing(c.path + ".inline") {
		return
	}
	depth := 1
	if parent != nil {
		depth = parent.inlineDepth + 1
	}
	scope := ""
	if parent != nil {
		scope = parent.scope
	}
	t, err := defaults.Template(g.workflow.Spec.TemplateDefaults, c.inline)
	if err != nil {
		g.refuse(c.path, "native")
		return
	}
	n := &node{key: c.path, scope: scope, path: c.path + ".inline", template: t, inlineDepth: depth, referenceDepth: g.scopes[scope].depth}
	c.target = n
	g.byKey[n.key] = n
	g.nodes = append(g.nodes, n)
}
func (g *graph) load(name, where string, depth int) {
	if g.objects[name] != nil {
		return
	}
	if err := g.ctx.Err(); err != nil {
		g.reads = append(g.reads, err)
		return
	}
	obj, err := g.readTemplate(name)
	if err != nil {
		g.reads = append(g.reads, templateReadFailure(name, where, err))
		return
	}
	if obj == nil {
		if g.policy.TemplateGetter != nil {
			g.refuse(where, "template_missing")
		} else {
			g.refuse(where, "live")
		}
		return
	}
	if obj.Name != name || obj.Namespace != g.request.IdentityRequest.Namespace {
		g.refuse(where, "reference")
		return
	}
	raw, err := json.Marshal(obj.Spec)
	if err != nil || len(raw) > admissioncontract.MaxDocumentBytes {
		g.refuse(where, "template_size")
		return
	}
	g.scopes[name] = &templateScope{depth: depth}
	g.objects[name] = obj.DeepCopy()
	g.snapshots = append(g.snapshots, admissioncontract.TemplateSnapshot{Namespace: obj.Namespace, Name: name, UID: string(obj.UID), ResourceVersion: obj.ResourceVersion, Spec: raw})
	g.addScope(name, obj.Spec, "templates["+name+"]")
}
func (g *graph) readTemplate(name string) (*wfv1.WorkflowTemplate, error) {
	if read, ok := g.attempts[name]; ok {
		return read.object, read.err
	}
	obj, err := g.fetchTemplate(name)
	if obj != nil {
		obj = obj.DeepCopy()
	}
	g.attempts[name] = templateRead{obj, err}
	return obj, err
}
func (g *graph) fetchTemplate(name string) (*wfv1.WorkflowTemplate, error) {
	for _, s := range g.request.Templates {
		if s.Name != name {
			continue
		}
		var spec wfv1.WorkflowSpec
		if len(s.Spec) > admissioncontract.MaxDocumentBytes {
			return nil, refusal("templates["+name+"]", "template_size")
		}
		if json.Unmarshal(s.Spec, &spec) != nil {
			return nil, refusal("templates["+name+"]", "template_yaml")
		}
		obj := &wfv1.WorkflowTemplate{Spec: spec}
		obj.Name = s.Name
		obj.Namespace = s.Namespace
		obj.UID = types.UID(s.UID)
		obj.ResourceVersion = s.ResourceVersion
		if !g.offline() {
			break
		}
		g.refuse("templates["+name+"]", "live")

		return obj, nil
	}
	if g.policy.TemplateGetter == nil {
		return nil, nil
	}
	obj, err := g.policy.TemplateGetter(g.ctx, name)
	if err != nil {
		return nil, err
	}
	return obj, nil
}
func literal(s string) bool { return s != "" && !strings.ContainsAny(s, "{}\x00\r\n") }

func templateKey(scope, name string) string {
	if scope == "" {
		return name
	}
	return scope + "/" + name
}

func (g *graph) continuing() bool {
	if g.stopped {
		return false
	}
	if err := g.ctx.Err(); err != nil {
		g.stopped = true
		g.reads = append(g.reads, err)
		return false
	}
	return true
}
func (g *graph) constructing(where string) bool {
	if !g.continuing() {
		return false
	}
	if g.constructions >= maxInspectionNodes {
		g.stopped = true
		g.refuse(where, "inspection")
		return false
	}
	g.constructions++
	return true
}
