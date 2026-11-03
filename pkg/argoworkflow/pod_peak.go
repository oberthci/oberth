package argoworkflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// PodDemand contains CPU requests and memory reservations, never observed usage. Each
// dimension is an independent upper bound; its peak need not occur together.
type PodDemand struct {
	// DeclaredTotal sums static pod invocations before dependency/parallelism
	// reductions. Nil means the total is unknown; it never sizes admission.
	DeclaredTotal     *PodRequests       `json:"declared_total,omitempty"`
	ExtendedResources map[string]int64   `json:"extended_resources,omitempty"`
	CPUMilli          int64              `json:"cpu_milli"`
	MemoryBytes       int64              `json:"memory_bytes"`
	Pods              int64              `json:"pods"`
	Assumptions       PodPeakAssumptions `json:"assumptions,omitempty"`
}

// Equal compares the capacity contract; optional diagnostic totals do not
// change historical reservation equality or permit a different peak.
func (d PodDemand) Equal(other PodDemand) bool {
	return d.CPUMilli == other.CPUMilli && d.MemoryBytes == other.MemoryBytes && d.Pods == other.Pods && d.Assumptions == other.Assumptions && maps.Equal(d.ExtendedResources, other.ExtendedResources)
}

func (d PodDemand) Valid() bool {
	if ValidateExtendedResourceLimits(d.ExtendedResources) != nil {
		return false
	}
	return validPodTotal(d.DeclaredTotal) && d.Assumptions.Valid() && d.CPUMilli > 0 && d.CPUMilli <= 1_000_000_000 && d.MemoryBytes > 0 && d.MemoryBytes <= 1<<60 && d.Pods > 0 && d.Pods <= 256
}

const maxPodGraphWork = 4096

var errPodGraph = errors.New("declared resource graph cannot be bounded; use static calls or positive workflow parallelism")
var errPodUndeclared = errors.New("pod has undeclared CPU/memory requests")

// ErrMemoryUnbounded refuses numeric admission for request-only containers.
// A reservation is not a runtime bound; never infer or insert a missing limit.
var ErrMemoryUnbounded = errors.New("pod lacks a positive declared memory limit")

type podGraph struct {
	uncapped     bool // diagnostic static total; still subject to graph work bounds
	resourcePods bool
	parallelism  int64
	limited      bool
	sequence     bool
	leaves       []PodDemand
	assumptions  PodPeakAssumptions
	templates    map[string]*wfv1.Template
	defaults     *wfv1.Template
	stack        map[*wfv1.Template]bool
	edges        [][]int
	demands      []PodDemand
	work         int
}

func (g *podGraph) node(d PodDemand) (int, error) {
	g.work++
	if g.work > maxPodGraphWork || len(g.edges) >= 256 {
		return 0, errPodGraph
	}
	n := len(g.edges)
	g.edges = append(g.edges, nil)
	g.demands = append(g.demands, d)
	return n, nil
}

func containerDemand(c, defaults *corev1.Container) (PodDemand, error) {
	d := PodDemand{Pods: 1}
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		q, ok := c.Resources.Requests[name]
		if !ok && defaults != nil {
			q, ok = defaults.Resources.Requests[name]
		}
		// Missing requests already select size admission. Report that cause
		// before testing whether a fully requested container has a limit.
		if !ok || q.Sign() <= 0 {
			return PodDemand{}, errPodUndeclared
		}
		if name == corev1.ResourceMemory {
			limit, set := c.Resources.Limits[name]
			if !set && defaults != nil {
				limit, set = defaults.Resources.Limits[name]
			}
			if !set || limit.Sign() <= 0 {
				return PodDemand{}, ErrMemoryUnbounded
			}
			if limit.CmpInt64(1<<40) > 0 {
				return PodDemand{}, errPodGraph
			}
			// Requests are reviewed scheduling demand. Limits remain runtime
			// ceilings; admission holds separate administrator-owned headroom.
		}
		if name == corev1.ResourceCPU {
			if q.CmpInt64(1000) > 0 {
				return PodDemand{}, errPodGraph
			}
			d.CPUMilli = q.MilliValue()
		} else {
			if q.CmpInt64(1<<40) > 0 {
				return PodDemand{}, errPodGraph
			}
			d.MemoryBytes = q.Value()
		}
	}
	var err error
	d.ExtendedResources, err = containerExtendedDemand(c, defaults)
	if err != nil {
		return PodDemand{}, err
	}
	if !d.Valid() {
		return PodDemand{}, errPodGraph
	}
	return d, nil
}

func addPodDemand(a, b PodDemand) PodDemand {
	return PodDemand{ExtendedResources: sumExtended(a.ExtendedResources, b.ExtendedResources), CPUMilli: a.CPUMilli + b.CPUMilli, MemoryBytes: a.MemoryBytes + b.MemoryBytes, Pods: max(a.Pods, b.Pods)}
}

type podContainer struct {
	container, defaults *corev1.Container
}

// Template defaults merge helper lists by name. Check inherited limits on the
// effective helper, and count that helper once rather than appending duplicates.
func podHelpers(authored, inherited []wfv1.UserContainer) []podContainer {
	defaults := map[string]*corev1.Container{}
	for i := range inherited {
		defaults[inherited[i].Name] = &inherited[i].Container
	}
	shadowed := map[string]bool{}
	var out []podContainer
	for i := range authored {
		c := &authored[i].Container
		out = append(out, podContainer{c, defaults[c.Name]})
		shadowed[c.Name] = true
	}
	for i := range inherited {
		c := &inherited[i].Container
		if !shadowed[c.Name] {
			out = append(out, podContainer{container: c})
		}
	}
	return out
}

func templatePodDemand(t, defaults *wfv1.Template, resourcePods bool) (PodDemand, error) {
	resourceOnly := resourcePods && t.Resource != nil
	var main, inherited *corev1.Container
	switch {
	case t.Container != nil:
		main = t.Container
		if defaults != nil {
			inherited = defaults.Container
		}
	case t.Script != nil:
		main = &t.Script.Container
		if defaults != nil && defaults.Script != nil {
			inherited = &defaults.Script.Container
		}
	case t.ContainerSet == nil:
		if !resourceOnly {
			return PodDemand{}, errPodGraph
		}
	}
	var d PodDemand
	if main != nil {
		var err error
		d, err = containerDemand(main, inherited)
		if err != nil {
			return d, err
		}
	} else if t.ContainerSet != nil {
		// Container-set lists replace defaults; unlike sidecars/init they do
		// not merge by name. Each authored member needs its own limit.
		for _, c := range t.ContainerSet.Containers {
			n, err := containerDemand(&c.Container, nil)
			if err != nil {
				return d, err
			}
			d = addPodDemand(d, n)
		}
	}
	// Resource executor capacity is held by the separate helper reserve; its
	// authored/inherited init containers and sidecars still require limits.
	// Sidecars stay resident. Summing all init requests is conservative for
	// both sequential init and restartable init; never subtract their requests.
	var sidecars, init, restartable PodDemand
	var defaultSidecars, defaultInit []wfv1.UserContainer
	if defaults != nil {
		defaultSidecars, defaultInit = defaults.Sidecars, defaults.InitContainers
	}
	for _, c := range podHelpers(t.Sidecars, defaultSidecars) {
		n, err := containerDemand(c.container, c.defaults)
		if err != nil {
			return d, err
		}
		sidecars = addPodDemand(sidecars, n)
	}
	for _, c := range podHelpers(t.InitContainers, defaultInit) {
		n, err := containerDemand(c.container, c.defaults)
		if err != nil {
			return d, err
		}
		init = addPodDemand(init, n)
		restart := c.container.RestartPolicy
		if restart == nil && c.defaults != nil {
			restart = c.defaults.RestartPolicy
		}
		if restart != nil && *restart == corev1.ContainerRestartPolicyAlways {
			restartable = addPodDemand(restartable, n)
		}
	}
	d.ExtendedResources = maxExtended(sumExtended(sumExtended(d.ExtendedResources, sidecars.ExtendedResources), restartable.ExtendedResources), init.ExtendedResources)
	d.CPUMilli = max(d.CPUMilli+sidecars.CPUMilli+restartable.CPUMilli, init.CPUMilli)
	d.MemoryBytes = max(d.MemoryBytes+sidecars.MemoryBytes+restartable.MemoryBytes, init.MemoryBytes)
	d.Pods = 1
	if d.CPUMilli > 1_000_000 || d.MemoryBytes > 1<<40 {
		return PodDemand{}, errPodGraph
	}
	return d, nil
}

func (g *podGraph) expand(t *wfv1.Template, depth int) (int, int, error) {
	if t == nil || depth > 32 || g.stack[t] || (t.Daemon != nil && *t.Daemon) {
		return 0, 0, errPodGraph
	}
	g.stack[t] = true
	defer delete(g.stack, t)
	start, err := g.node(PodDemand{})
	if err != nil {
		return 0, 0, err
	}
	if t.DAG == nil && len(t.Steps) == 0 {
		d, err := templatePodDemand(t, g.defaults, g.resourcePods)
		g.demands[start] = d
		g.leaves = append(g.leaves, d)
		return start, start, err
	}
	end, err := g.node(PodDemand{})
	if err != nil {
		return 0, 0, err
	}
	limit := t.Parallelism
	if limit == nil && g.defaults != nil {
		limit = g.defaults.Parallelism
	}
	var localLimit int64
	if !g.uncapped && limit != nil && *limit > 0 && *limit <= 256 {
		localLimit = *limit
		g.limited = true
	}
	fanoutLimit := localLimit
	if g.parallelism > 0 && (fanoutLimit == 0 || g.parallelism < fanoutLimit) {
		fanoutLimit = g.parallelism
	}
	var children []PodDemand
	expandChild := func(child *wfv1.Template) (int, int, error) {
		first := len(g.edges)
		a, b, err := g.expand(child, depth+1)
		if err != nil || localLimit == 0 {
			return a, b, err
		}
		d, err := g.peak(first, len(g.edges))
		if err != nil {
			return 0, 0, err
		}
		children = append(children, d)
		if child.DAG != nil || len(child.Steps) > 0 {
			g.assumptions |= PodPeakNested
		}
		return a, b, nil
	}
	call := func(name string, inline *wfv1.Template, ref *wfv1.TemplateRef, exit string, hooks wfv1.LifecycleHooks, copies int) (int, int, error) {
		if ref != nil || exit != "" || len(hooks) != 0 {
			return 0, 0, errPodGraph
		}
		child := g.templates[name]
		if inline != nil {
			child = inline
		}
		if copies < 0 {
			if fanoutLimit == 0 {
				return 0, 0, errPodGraph
			}
			copies = int(fanoutLimit)
			g.assumptions |= PodPeakDynamic
		} else if fanoutLimit > 0 {
			copies = min(copies, int(fanoutLimit))
		} else if copies > 128 {
			return 0, 0, errPodGraph
		}
		if copies == 1 {
			return expandChild(child)
		}
		a, err := g.node(PodDemand{})
		if err != nil {
			return 0, 0, err
		}
		b, err := g.node(PodDemand{})
		if err != nil {
			return 0, 0, err
		}
		g.edges[a] = append(g.edges[a], b)
		for range copies {
			x, y, e := expandChild(child)
			if e != nil {
				return 0, 0, e
			}
			g.edges[a] = append(g.edges[a], x)
			g.edges[y] = append(g.edges[y], b)
		}
		return a, b, nil
	}
	if t.DAG != nil {
		starts, ends := map[string]int{}, map[string]int{}
		deps := map[string][]string{}
		for _, task := range t.DAG.Tasks {
			g.sequence = g.sequence || task.WithSequence != nil
			if task.When != "" {
				g.assumptions |= PodPeakConditions
			}
			if _, duplicate := starts[task.Name]; duplicate {
				return 0, 0, errPodGraph
			}
			a, b, e := call(task.Template, task.Inline, task.TemplateRef, task.OnExit, task.Hooks, podFanoutCount(task.WithItems, task.WithParam, task.WithSequence))
			if e != nil {
				return 0, 0, e
			}
			starts[task.Name], ends[task.Name] = a, b
			deps[task.Name] = task.Dependencies
			if task.Depends != "" {
				// Recognize only proven completion prerequisites. Terminal OR
				// groups for the same predecessor serialize ordinary pods too.
				// Mixed-task OR, negation and early-start logic earn no credit.
				deps[task.Name] = nil
				if completed, ok := podCompletionDependencies(task.Depends); ok {
					for _, dep := range completed {
						deps[task.Name] = append(deps[task.Name], dep)
					}
				} else {
					g.assumptions |= PodPeakConditions
				}
			}
		}
		for name, a := range starts {
			g.edges[start] = append(g.edges[start], a)
			g.edges[ends[name]] = append(g.edges[ends[name]], end)
			for _, dep := range deps[name] {
				b, ok := ends[dep]
				if !ok {
					return 0, 0, errPodGraph
				}
				g.edges[b] = append(g.edges[b], a)
			}
		}
	} else {
		previous := []int{start}
		for _, group := range t.Steps {
			next := []int{}
			for _, step := range group.Steps {
				g.sequence = g.sequence || step.WithSequence != nil
				if step.When != "" {
					g.assumptions |= PodPeakConditions
				}
				a, b, e := call(step.Template, step.Inline, step.TemplateRef, step.OnExit, step.Hooks, podFanoutCount(step.WithItems, step.WithParam, step.WithSequence))
				if e != nil {
					return 0, 0, e
				}
				for _, p := range previous {
					g.edges[p] = append(g.edges[p], a)
				}
				next = append(next, b)
			}
			previous = next
		}
		for _, p := range previous {
			g.edges[p] = append(g.edges[p], end)
		}
	}
	if localLimit > 0 {
		d, err := g.peak(start, len(g.edges))
		if err != nil {
			return 0, 0, err
		}
		// Template parallelism limits direct child invocations, not all pods
		// recursively. Each child keeps its full independently bounded demand.
		d.ExtendedResources = limitExtendedPeak(d.ExtendedResources, children, int(localLimit))
		cpu, memory, pods := demandVectors(children)
		d.CPUMilli = min(d.CPUMilli, largestPodRequests(cpu, int(localLimit)))
		d.MemoryBytes = min(d.MemoryBytes, largestPodRequests(memory, int(localLimit)))
		d.Pods = min(d.Pods, largestPodRequests(pods, int(localLimit)))
		g.edges = g.edges[:start+1]
		g.edges[start] = nil
		g.demands = g.demands[:start+1]
		g.demands[start] = d
		return start, start, nil
	}
	return start, end, nil
}

// DeclaredPodPeak returns nil for incomplete requests and ErrMemoryUnbounded
// for a missing effective memory limit. Workflow and template parallelism bound
// fan-out at their respective scopes. Unsupported graphs require a workflow
// bound and reserve the largest possible pod per slot. Mutable item counts and
// conditions earn no reductions beyond their enforced concurrency bounds.
func DeclaredPodPeak(wf *wfv1.Workflow) (*PodDemand, error) { return declaredPodPeak(wf, false) }

// DeclaredCapabilityPeak includes resource executors as bounded helper pods;
// their opaque objects are constrained separately by operator namespace quota.
func DeclaredCapabilityPeak(wf *wfv1.Workflow) (*PodDemand, error) {
	return declaredPodPeak(wf, true)
}

func declaredPodPeak(wf *wfv1.Workflow, resourcePods bool) (*PodDemand, error) {
	if wf != nil && wf.Spec.TemplateDefaults != nil && wf.Spec.TemplateDefaults.Daemon != nil && *wf.Spec.TemplateDefaults.Daemon {
		return nil, errPodGraph
	}
	c := ResourceCoverage(wf)
	if c == nil {
		return nil, errPodGraph
	}
	if c.Undeclared != 0 || (c.Declared == 0 && !resourcePods) {
		return nil, nil
	}
	var parallelism int64
	if p := wf.Spec.Parallelism; p != nil && *p > 0 && *p <= 256 {
		parallelism = *p
	}
	g := podGraph{parallelism: parallelism, resourcePods: resourcePods, templates: map[string]*wfv1.Template{}, defaults: wf.Spec.TemplateDefaults, stack: map[*wfv1.Template]bool{}}
	for i := range wf.Spec.Templates {
		t := &wf.Spec.Templates[i]
		g.templates[t.Name] = t
	}
	_, _, err := g.expand(g.templates[wf.Spec.Entrypoint], 0)
	if errors.Is(err, errPodUndeclared) {
		return nil, nil
	}
	if errors.Is(err, ErrMemoryUnbounded) {
		return nil, err
	}
	if wf.Spec.OnExit != "" || len(wf.Spec.Hooks) != 0 {
		return nil, errPodGraph
	}
	// Sequences require an effective parallelism limit for bounded analysis.
	if err != nil || g.sequence && !g.limited && parallelism == 0 {
		return boundedPodPeak(wf, resourcePods)
	}
	d, err := g.peak(0, len(g.edges))
	if err != nil {
		return nil, err
	}
	if parallelism > 0 && parallelism < d.Pods {
		// Use actual leaf weights, never a collapsed nested template envelope,
		// for the workflow-wide pod cap.
		d.ExtendedResources = limitExtendedPeak(d.ExtendedResources, g.leaves, int(parallelism))
		cpu, memory, _ := demandVectors(g.leaves)
		d.CPUMilli = min(d.CPUMilli, largestPodRequests(cpu, int(parallelism)))
		d.MemoryBytes = min(d.MemoryBytes, largestPodRequests(memory, int(parallelism)))
		d.Pods = parallelism
	}
	d.DeclaredTotal = declaredPodTotal(wf, resourcePods)
	d.Assumptions = g.assumptions
	emptyExecutor := resourcePods && d.Pods > 0 && d.CPUMilli == 0 && d.MemoryBytes == 0
	if !d.Valid() && !emptyExecutor {
		return nil, errPodGraph
	}
	return &d, nil
}

// peak preserves dependency ordering within a subgraph. Collapsed bounded
// templates carry independent upper bounds in each dimension.
func (g *podGraph) peak(first, end int) (PodDemand, error) {
	n := end - first
	reach := make([][]bool, n)
	for i := range reach {
		reach[i] = make([]bool, n)
		for _, j := range g.edges[first+i] {
			if j >= first && j < end {
				reach[i][j-first] = true
			}
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
			return PodDemand{}, errPodGraph
		}
	}
	cpu, memory, pods := demandVectors(g.demands[first:end])
	return PodDemand{ExtendedResources: peakExtended(reach, g.demands[first:end]), CPUMilli: maxWeightedAntichain(reach, cpu), MemoryBytes: maxWeightedAntichain(reach, memory), Pods: maxWeightedAntichain(reach, pods)}, nil
}

func demandVectors(demands []PodDemand) (cpu, memory, pods []int64) {
	for _, d := range demands {
		cpu = append(cpu, d.CPUMilli)
		memory = append(memory, d.MemoryBytes)
		pods = append(pods, d.Pods)
	}
	return
}

func largestPodRequests(values []int64, count int) int64 {
	sort.Slice(values, func(i, j int) bool { return values[i] > values[j] })
	var n int64
	for _, v := range values[:min(count, len(values))] {
		n += v
	}
	return n
}

func boundedPodPeak(wf *wfv1.Workflow, resourcePods bool) (*PodDemand, error) {
	p := wf.Spec.Parallelism
	if p == nil || *p <= 0 || *p > 256 {
		return nil, errPodGraph
	}
	var maximum PodDemand
	templates := map[string]*wfv1.Template{}
	for i := range wf.Spec.Templates {
		templates[wf.Spec.Templates[i].Name] = &wf.Spec.Templates[i]
	}
	seen := map[*wfv1.Template]bool{}
	var scan func(*wfv1.Template, int) error
	work := 0
	scan = func(t *wfv1.Template, depth int) error {
		if t == nil {
			return errPodGraph
		}
		if seen[t] {
			return nil
		}
		seen[t] = true
		work++
		if depth > 32 || work > maxPodGraphWork || (t.Daemon != nil && *t.Daemon) {
			return errPodGraph
		}
		if t.Container != nil || t.Script != nil || t.ContainerSet != nil || resourcePods && t.Resource != nil {
			d, err := templatePodDemand(t, wf.Spec.TemplateDefaults, resourcePods)
			if err != nil {
				return err
			}
			maximum.ExtendedResources = maxExtended(maximum.ExtendedResources, d.ExtendedResources)
			maximum.CPUMilli = max(maximum.CPUMilli, d.CPUMilli)
			maximum.MemoryBytes = max(maximum.MemoryBytes, d.MemoryBytes)
		}
		tasks := []wfv1.DAGTask{}
		if t.DAG != nil {
			tasks = t.DAG.Tasks
		}
		for _, task := range tasks {
			if task.OnExit != "" || len(task.Hooks) != 0 {
				return errPodGraph
			}
			child := task.Inline
			if child == nil {
				child = templates[task.Template]
			}
			if err := scan(child, depth+1); err != nil {
				return err
			}
		}
		for _, group := range t.Steps {
			for _, step := range group.Steps {
				if step.OnExit != "" || len(step.Hooks) != 0 {
					return errPodGraph
				}
				child := step.Inline
				if child == nil {
					child = templates[step.Template]
				}
				if err := scan(child, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := scan(templates[wf.Spec.Entrypoint], 0); errors.Is(err, errPodUndeclared) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	d := PodDemand{ExtendedResources: scaleExtended(maximum.ExtendedResources, *p), CPUMilli: maximum.CPUMilli * *p, MemoryBytes: maximum.MemoryBytes * *p, Pods: *p, Assumptions: PodPeakGraph}
	if !d.Valid() {
		return nil, fmt.Errorf("%w: requests exceed graph bounds", errPodGraph)
	}
	return &d, nil
}

func podFanoutCount(items []wfv1.Item, param string, sequence *wfv1.Sequence) int {
	forms := 0
	if len(items) > 0 {
		forms++
	}
	if param != "" {
		forms++
	}
	if sequence != nil {
		forms++
	}
	if forms > 1 {
		// Conflicting loop sources must never earn the smaller cardinality.
		// Use the full enforced bound, or decline numeric admission.
		return -1
	}
	if sequence != nil {
		return podSequenceCount(sequence)
	}
	if param != "" {
		var values []json.RawMessage
		if json.Unmarshal([]byte(param), &values) != nil || !strings.HasPrefix(strings.TrimSpace(param), "[") {
			return -1
		}
		return len(values)
	}
	if len(items) > 0 {
		return len(items)
	}
	// An empty withItems is not expanded by the executor: the call runs once.
	return 1
}

// Saturate large literal sequences before expansion; limits can still bound
// their concurrency. Parse in int64 and compare before subtracting endpoints.
func podSequenceCount(sequence *wfv1.Sequence) int {
	parse := func(value string) (int64, error) { return strconv.ParseInt(value, 10, 64) }
	start := int64(0)
	var err error
	if sequence.Start != nil {
		start, err = parse(sequence.Start.String())
		if err != nil {
			return -1
		}
	}
	if sequence.End != nil {
		end, err := parse(sequence.End.String())
		if err != nil {
			return -1
		}
		lo, hi := min(start, end), max(start, end)
		if lo <= (1<<63-1)-maxPodGraphWork && hi > lo+maxPodGraphWork {
			return maxPodGraphWork + 1
		}
		return int(hi-lo) + 1
	}
	if sequence.Count != nil {
		count, err := parse(sequence.Count.String())
		if err != nil || count < 0 {
			return -1
		}
		return int(min(count, maxPodGraphWork+1))
	}
	return -1
}
