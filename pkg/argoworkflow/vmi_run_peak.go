package argoworkflow

import (
	"slices"
	"strconv"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

const (
	maxVMIProjectionWork  = 4096
	maxVMIPartitionValues = 32
)

type runVMIEvent struct {
	vmiGraphEvent
	condition vmiCondition
}
type runVMIArc struct {
	to       int
	success  bool
	executed bool
}
type runVMIGraph struct {
	templates    map[string]*wfv1.Template
	pure         map[string]bool
	classified   map[string]bool
	dependencies map[string]map[string][]vmiDependency
	stack        map[string]bool
	work         int
	edges        [][]runVMIArc
	events       []runVMIEvent
	trigger      RunTrigger
}

func (g *runVMIGraph) spend(n int) bool { g.work += n; return g.work <= maxVMIProjectionWork }
func (g *runVMIGraph) node() (int, bool) {
	if !g.spend(1) || len(g.edges) >= maxVMIGraphNodes {
		return 0, false
	}
	n := len(g.edges)
	g.edges = append(g.edges, nil)
	return n, true
}
func (g *runVMIGraph) edge(a, b int, success, executed bool) {
	g.edges[a] = append(g.edges[a], runVMIArc{b, success, executed})
}

func boundedVMIRetry(retry *wfv1.RetryStrategy) bool {
	if retry == nil {
		return true
	}
	if retry.Limit == nil {
		return false
	}
	limit, err := strconv.Atoi(retry.Limit.String())
	return err == nil && limit >= 0 && limit <= MaxRetryLimit
}

func staticVMITask(name, template string, ref *wfv1.TemplateRef, inline *wfv1.Template, onExit string, hooks wfv1.LifecycleHooks, fanout bool, continueOn *wfv1.ContinueOn) bool {
	return vmiDNSLabelPattern.MatchString(name) && template != "" && ref == nil && inline == nil && onExit == "" && len(hooks) == 0 && !fanout && continueOn == nil
}

// Memoized classification inspects ALL reachable descendants, before any
// collapse. Bounded retries are safe only for wholly lifetime-neutral graphs.
// The scan budget is separate from the much smaller projected-graph budget.
func (g *runVMIGraph) classify(name string) (bool, bool) {
	if !g.spend(1) || g.stack[name] {
		return false, false
	}
	if g.classified[name] {
		return g.pure[name], true
	}
	t, ok := g.templates[name]
	if !ok || t.Daemon != nil {
		return false, false
	}
	g.stack[name] = true
	defer delete(g.stack, name)
	pure := true
	if t.Resource != nil {
		op, err := VMIOperation(t.Resource)
		if err != nil {
			return false, false
		}
		pure = op.Action == "get"
	}
	if t.DAG != nil {
		if len(t.Steps) != 0 || t.DAG.Target != "" || len(t.DAG.Tasks) == 0 {
			return false, false
		}
		for _, task := range t.DAG.Tasks {
			if !staticVMITask(task.Name, task.Template, task.TemplateRef, task.Inline, task.OnExit, task.Hooks, len(task.WithItems) > 0 || task.WithParam != "" || task.WithSequence != nil, task.ContinueOn) {
				return false, false
			}
			child, ok := g.classify(task.Template)
			if !ok {
				return false, false
			}
			pure = pure && child
		}
		deps, ok := runVMIDAGDependencies(t.DAG)
		if !ok {
			return false, false
		}
		g.dependencies[name] = deps
	}
	for _, group := range t.Steps {
		if len(group.Steps) == 0 {
			return false, false
		}
		for _, step := range group.Steps {
			if !staticVMITask(step.Name, step.Template, step.TemplateRef, step.Inline, step.OnExit, step.Hooks, len(step.WithItems) > 0 || step.WithParam != "" || step.WithSequence != nil, step.ContinueOn) {
				return false, false
			}
			child, ok := g.classify(step.Template)
			if !ok {
				return false, false
			}
			pure = pure && child
		}
	}
	if t.RetryStrategy != nil && (!pure || !boundedVMIRetry(t.RetryStrategy)) {
		return false, false
	}
	g.pure[name], g.classified[name] = pure, true
	return pure, true
}

// Local dependency validation is also performed for graphs that are collapsed.
// A pure cycle or unresolved output producer cannot silently become a proof.
func runVMIDAGDependencies(dag *wfv1.DAGTemplate) (map[string][]vmiDependency, bool) {
	if len(dag.Tasks) > MaxDAGTasks {
		return nil, false
	}
	deps := map[string][]vmiDependency{}
	hasDepends, hasLegacy := false, false
	for _, task := range dag.Tasks {
		if _, exists := deps[task.Name]; exists {
			return nil, false
		}
		hasDepends = hasDepends || task.Depends != ""
		hasLegacy = hasLegacy || len(task.Dependencies) > 0
		if hasDepends && hasLegacy {
			return nil, false
		}
		var ds []vmiDependency
		if task.Depends != "" {
			var ok bool
			ds, ok = parseVMIDependencies(task.Depends)
			if !ok {
				return nil, false
			}
		} else {
			for _, dep := range task.Dependencies {
				ds = append(ds, vmiDependency{name: dep, bare: true})
			}
		}
		deps[task.Name] = ds
	}
	state := map[string]int{}
	var visit func(string) bool
	visit = func(name string) bool {
		ds, exists := deps[name]
		if !exists || state[name] == 1 {
			return false
		}
		if state[name] == 2 {
			return true
		}
		state[name] = 1
		for _, dep := range ds {
			if !visit(dep.name) {
				return false
			}
		}
		state[name] = 2
		return true
	}
	for name := range deps {
		if !visit(name) {
			return nil, false
		}
	}
	if !compileVMIArgoDependencies(dag) {
		return nil, false
	}
	return deps, true
}

// Order alone is insufficient for output arguments: Argo may substitute an
// absent-output sentinel for a Skipped/Omitted producer and then use each
// consumer's own input Default/Value. Require an explicit local Succeeded
// prerequisite before sharing one output identity across DAG consumers.
func vmiRequiresOutputSuccess(deps map[string][]vmiDependency, task, producer string) bool {
	for _, dep := range deps[task] {
		if dep.name == producer && dep.success {
			return true
		}
	}
	return false
}

func (g *runVMIGraph) outputScalars(name, path string, refs map[string]vmiScalar) {
	t := g.templates[name]
	if t == nil || !g.pure[name] {
		return
	}
	for _, p := range t.Outputs.Parameters {
		// A single named final scalar output belongs to this exact invocation.
		// No planner is executed or its value predicted before reservation.
		if p.ValueFrom != nil && p.ValueFrom.Path != "" {
			refs[p.Name] = vmiScalar{id: path + "/output/" + p.Name}
		}
	}
}

func runVMIInputs(args wfv1.Arguments, inputs wfv1.Inputs, resolve func(string) (vmiScalar, bool)) map[string]vmiScalar {
	env := map[string]vmiScalar{}
	seen := map[string]bool{}
	for _, p := range args.Parameters {
		if seen[p.Name] {
			delete(env, "inputs.parameters."+p.Name)
			continue
		}
		seen[p.Name] = true
		if p.Value == nil || p.ValueFrom != nil {
			continue
		}
		value := p.Value.String()
		if match := vmiScalarReference.FindStringSubmatch(value); len(match) == 2 {
			if scalar, ok := resolve(match[1]); ok {
				// A successful producer can itself emit Argo's absent-argument
				// sentinel. ProcessArgs then uses each consumer's Default/Value/
				// ValueFrom, so producer success alone cannot make those equal.
				if !scalar.constant {
					fallback := false
					for _, input := range inputs.Parameters {
						if input.Name == p.Name && (input.Default != nil || input.Value != nil || input.ValueFrom != nil) {
							fallback = true
						}
					}
					if fallback {
						continue
					}
				}
				env["inputs.parameters."+p.Name] = scalar
			}
		} else if vmiGuardLiteral.MatchString(value) {
			env["inputs.parameters."+p.Name] = vmiScalar{constant: true, literal: value}
		}
	}
	return env
}

func (g *runVMIGraph) expand(name, path string, inherited vmiCondition, inputs map[string]vmiScalar) (int, int, bool) {
	t := g.templates[name]
	start, ok := g.node()
	if !ok || t == nil {
		return 0, 0, false
	}
	if g.pure[name] {
		return start, start, true
	}
	if t.Resource != nil {
		op, err := VMIOperation(t.Resource)
		if err != nil {
			return 0, 0, false
		}
		g.events = append(g.events, runVMIEvent{vmiGraphEvent: vmiGraphEvent{node: start, template: name, name: op.Name, action: op.Action, demand: op.Demand}, condition: inherited})
		return start, start, true
	}
	end, ok := g.node()
	if !ok {
		return 0, 0, false
	}
	if t.DAG != nil {
		deps, ok := g.dependencies[name]
		if !ok {
			return 0, 0, false
		}
		starts, ends := map[string]int{}, map[string]int{}
		conditions := map[string]vmiCondition{}
		outputs := map[string]map[string]vmiScalar{}
		for _, task := range t.DAG.Tasks {
			outputs[task.Name] = map[string]vmiScalar{}
			g.outputScalars(task.Template, path+"/"+task.Name, outputs[task.Name])
		}
		for _, task := range t.DAG.Tasks {
			resolve := func(ref string) (vmiScalar, bool) {
				if scalar, ok := inputs[ref]; ok {
					return scalar, true
				}
				bits := strings.Split(ref, ".")
				if len(bits) != 5 || bits[0] != "tasks" || bits[2] != "outputs" || bits[3] != "parameters" || !vmiRequiresOutputSuccess(deps, task.Name, bits[1]) {
					return vmiScalar{}, false
				}
				scalar, ok := outputs[bits[1]][bits[4]]
				return scalar, ok
			}
			condition := vmiCombine(inherited, parseVMICondition(task.When, g.trigger, resolve, 0), true)
			a, b, ok := g.expand(task.Template, path+"/"+task.Name, condition, runVMIInputs(task.Arguments, g.templates[task.Template].Inputs, resolve))
			if !ok {
				return 0, 0, false
			}
			starts[task.Name], ends[task.Name], conditions[task.Name] = a, b, condition
		}
		for _, task := range t.DAG.Tasks {
			if len(deps[task.Name]) == 0 {
				g.edge(start, starts[task.Name], false, true)
			}
			for _, dep := range deps[task.Name] {
				// Bare/legacy dependencies accept Skipped. Only an unconditional
				// predecessor can make that shorthand a success requirement.
				strict := dep.success || (dep.bare && conditions[dep.name].truth == vmiTrue)
				g.edge(ends[dep.name], starts[task.Name], strict, strict || dep.executed)
			}
			// Composite completion is only an ordering barrier. Do not infer
			// successful execution of an omitted child from parent success.
			g.edge(ends[task.Name], end, false, false)
		}
	} else if len(t.Steps) > 0 {
		previous := []int{start}
		outputs := map[string]vmiScalar{}
		seen := map[string]bool{}
		for _, group := range t.Steps {
			next := []int{}
			newOutputs := map[string]vmiScalar{}
			for _, step := range group.Steps {
				if seen[step.Name] {
					return 0, 0, false
				}
				seen[step.Name] = true
				resolve := func(ref string) (vmiScalar, bool) {
					if s, ok := inputs[ref]; ok {
						return s, true
					}
					s, ok := outputs[ref]
					return s, ok
				}
				condition := vmiCombine(inherited, parseVMICondition(step.When, g.trigger, resolve, 0), true)
				a, b, ok := g.expand(step.Template, path+"/"+step.Name, condition, runVMIInputs(step.Arguments, g.templates[step.Template].Inputs, resolve))
				if !ok {
					return 0, 0, false
				}
				for _, p := range previous {
					g.edge(p, a, true, true)
				}
				next = append(next, b)
				// Later groups also run after skipped steps. Their arguments
				// can fall back to different consumer defaults, so only a
				// provably unconditional producer supplies a shared scalar.
				if condition.truth == vmiTrue {
					refs := map[string]vmiScalar{}
					g.outputScalars(step.Template, path+"/"+step.Name, refs)
					for name, s := range refs {
						newOutputs["steps."+step.Name+".outputs.parameters."+name] = s
					}
				}
			}
			for key, s := range newOutputs {
				outputs[key] = s
			}
			previous = next
		}
		for _, p := range previous {
			g.edge(p, end, false, false)
		}
	} else {
		return 0, 0, false
	}
	return start, end, true
}

func (g *runVMIGraph) cleanupOnly(name string, seen map[string]bool) bool {
	if !g.spend(1) || seen[name] {
		return false
	}
	t := g.templates[name]
	if t == nil {
		return false
	}
	seen[name] = true
	defer delete(seen, name)
	if t.Resource != nil && t.Resource.Action != "get" && t.Resource.Action != "delete" {
		return false
	}
	if t.DAG != nil {
		for _, task := range t.DAG.Tasks {
			if !g.cleanupOnly(task.Template, seen) {
				return false
			}
		}
	}
	for _, group := range t.Steps {
		for _, step := range group.Steps {
			if !g.cleanupOnly(step.Template, seen) {
				return false
			}
		}
	}
	return true
}

func runVMIPeak(wf *wfv1.Workflow, trigger RunTrigger) (VMIDemand, bool) {
	if !trigger.Valid() || wf.Spec.TemplateDefaults != nil || wf.Spec.RetryStrategy != nil || len(wf.Spec.Hooks) > 0 {
		return VMIDemand{}, false
	}
	g := runVMIGraph{templates: map[string]*wfv1.Template{}, pure: map[string]bool{}, classified: map[string]bool{}, dependencies: map[string]map[string][]vmiDependency{}, stack: map[string]bool{}, trigger: trigger}
	for i := range wf.Spec.Templates {
		t := &wf.Spec.Templates[i]
		if g.templates[t.Name] != nil {
			return VMIDemand{}, false
		}
		g.templates[t.Name] = t
	}
	if _, ok := g.classify(wf.Spec.Entrypoint); !ok {
		return VMIDemand{}, false
	}
	if wf.Spec.OnExit != "" {
		if _, ok := g.classify(wf.Spec.OnExit); !ok || !g.cleanupOnly(wf.Spec.OnExit, map[string]bool{}) {
			return VMIDemand{}, false
		}
	}
	if _, _, ok := g.expand(wf.Spec.Entrypoint, "entry", vmiCondition{truth: vmiTrue}, nil); !ok {
		return VMIDemand{}, false
	}
	selectors := map[string][]string{}
	for _, e := range g.events {
		if e.condition.unsafe {
			return VMIDemand{}, false
		}
		if e.condition.scalar != "" {
			values := selectors[e.condition.scalar]
			if !slices.Contains(values, e.condition.literal) {
				selectors[e.condition.scalar] = append(values, e.condition.literal)
			}
		}
	}
	// One immutable invocation scalar is sufficient for a partition proof.
	// Multiple independent selectors get no mutual-exclusion credits.
	selector, values := "", []string{""}
	if len(selectors) > 1 {
		return VMIDemand{}, false
	}
	if len(selectors) == 1 {
		for id, literals := range selectors {
			if len(literals) > maxVMIPartitionValues {
				return VMIDemand{}, false
			}
			selector = id
			values = append(values, literals...)
		}
	}
	// Bound enabled create invocations and charge scenario scans BEFORE the
	// reachability matrices. This also rejects a large parallel graph before
	// paying for its transitive closure, irrespective of partition count.
	scenarios := make([][]runVMIEvent, len(values))
	for i, value := range values {
		for _, e := range g.events {
			if !g.spend(1) {
				return VMIDemand{}, false
			}
			if e.action == "create" && e.condition.evaluate(selector, value) != vmiFalse {
				scenarios[i] = append(scenarios[i], e)
				if len(scenarios[i]) > VMIBudgetLimit().MaxVMs {
					return VMIDemand{}, false
				}
			}
		}
	}
	n := len(g.edges)
	order, execution := make([][]bool, n), make([][]bool, n)
	for i := range n {
		order[i] = make([]bool, n)
		execution[i] = make([]bool, n)
		for _, arc := range g.edges[i] {
			order[i][arc.to] = true
			execution[i][arc.to] = execution[i][arc.to] || arc.executed
		}
	}
	for k := range n {
		for i := range n {
			for j := range n {
				order[i][j] = order[i][j] || (order[i][k] && order[k][j])
				execution[i][j] = execution[i][j] || (execution[i][k] && execution[k][j])
			}
		}
	}
	for i := range n {
		if order[i][i] {
			return VMIDemand{}, false
		}
	}
	// A success edge requires its predecessor's Succeeded status. Later edges
	// need only require actual execution: a failed driver still inherits the
	// successful delete barriers that were necessary before it could start.
	// Five-state terminal groups can accept Omitted/Skipped and break that
	// propagation. All DAG depends expressions have first passed the pinned
	// Argo compiler: an Errored successor must not arise from a syntax/type
	// error before its depends gate. Terminal ordering alone proves no execution.
	success := make([][]bool, n)
	for i := range n {
		success[i] = make([]bool, n)
		for _, arc := range g.edges[i] {
			if arc.success {
				success[i][arc.to] = true
				for j := range n {
					success[i][j] = success[i][j] || execution[arc.to][j]
				}
			}
		}
	}
	var peak VMIDemand
	for i, value := range values {
		demand, ok := g.scenarioPeak(order, success, selector, value, scenarios[i])
		if !ok {
			return VMIDemand{}, false
		}
		peak.VMs = max(peak.VMs, demand.VMs)
		peak.Cores = max(peak.Cores, demand.Cores)
		peak.MemoryBytes = max(peak.MemoryBytes, demand.MemoryBytes)
	}
	return peak, true
}

func (g *runVMIGraph) scenarioPeak(order, success [][]bool, selector, value string, creates []runVMIEvent) (VMIDemand, bool) {
	// Inspect only matching deletes rather than scanning every event for each
	// create pair. All scenario scans and proof checks share the work budget.
	deletes := map[string][]runVMIEvent{}
	for _, e := range g.events {
		if !g.spend(1) {
			return VMIDemand{}, false
		}
		if e.action == "delete" && e.condition.evaluate(selector, value) == vmiTrue {
			deletes[e.name] = append(deletes[e.name], e)
		}
	}
	before := make([][]bool, len(creates))
	vms, cores, memory := make([]int64, len(creates)), make([]int64, len(creates)), make([]int64, len(creates))
	for i, a := range creates {
		vms[i], cores[i], memory[i] = 1, int64(a.demand.Cores), a.demand.MemoryBytes
		before[i] = make([]bool, len(creates))
		for j, b := range creates {
			if !g.spend(1) {
				return VMIDemand{}, false
			}
			if i == j {
				continue
			}
			for _, d := range deletes[a.name] {
				if !g.spend(1) {
					return VMIDemand{}, false
				}
				if order[a.node][d.node] && success[d.node][b.node] {
					before[i][j] = true
					break
				}
			}
		}
	}
	// Dilworth requires a partial order. Conditional execution can otherwise
	// create A<B and B<C without proving A<C (B may be omitted on the path
	// that reaches C). Adding transitive edges would invent deletion credits.
	for i := range creates {
		for j := range creates {
			for k := range creates {
				if !g.spend(1) {
					return VMIDemand{}, false
				}
				if before[i][j] && before[j][k] && !before[i][k] {
					return VMIDemand{}, false
				}
			}
		}
	}
	return VMIDemand{VMs: int(maxWeightedVMAntichain(before, vms)), Cores: int(maxWeightedVMAntichain(before, cores)), MemoryBytes: maxWeightedVMAntichain(before, memory)}, true
}
