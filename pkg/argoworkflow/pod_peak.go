package argoworkflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// PodDemand contains CPU requests and memory reservations, never observed usage. Each
// dimension is an independent upper bound; its peak need not occur together.
type PodDemand struct {
	CPUMilli    int64 `json:"cpu_milli"`
	MemoryBytes int64 `json:"memory_bytes"`
	Pods        int64 `json:"pods"`
}

func (d PodDemand) Valid() bool {
	return d.CPUMilli > 0 && d.CPUMilli <= 1_000_000_000 && d.MemoryBytes > 0 && d.MemoryBytes <= 1<<60 && d.Pods > 0 && d.Pods <= 256
}

const maxPodGraphWork = 4096

var errPodGraph = errors.New("declared resource graph cannot be bounded; use static calls or positive workflow parallelism")
var errPodUndeclared = errors.New("pod has undeclared CPU/memory requests")

type podGraph struct {
	resourcePods bool
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
		if name == corev1.ResourceMemory {
			limit, set := c.Resources.Limits[name]
			if !set && defaults != nil {
				limit, set = defaults.Resources.Limits[name]
			}
			if set && limit.Sign() > 0 && (!ok || limit.Cmp(q) > 0) {
				q = limit
				ok = true
			}
		}
		if !ok || q.Sign() <= 0 {
			return PodDemand{}, errPodUndeclared
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
	if !d.Valid() {
		return PodDemand{}, errPodGraph
	}
	return d, nil
}

func addPodDemand(a, b PodDemand) PodDemand {
	return PodDemand{CPUMilli: a.CPUMilli + b.CPUMilli, MemoryBytes: a.MemoryBytes + b.MemoryBytes, Pods: max(a.Pods, b.Pods)}
}

func templatePodDemand(t, defaults *wfv1.Template, resourcePods bool) (PodDemand, error) {
	if resourcePods && t.Resource != nil {
		return PodDemand{Pods: 1}, nil
	}
	var main, inherited *corev1.Container
	if defaults != nil {
		inherited = defaults.Container
		if defaults.Script != nil {
			inherited = &defaults.Script.Container
		}
	}
	switch {
	case t.Container != nil:
		main = t.Container
	case t.Script != nil:
		main = &t.Script.Container
	case t.ContainerSet == nil:
		return PodDemand{}, errPodGraph
	}
	var d PodDemand
	if main != nil {
		var err error
		d, err = containerDemand(main, inherited)
		if err != nil {
			return d, err
		}
	} else {
		for _, c := range t.ContainerSet.Containers {
			n, err := containerDemand(&c.Container, nil)
			if err != nil {
				return d, err
			}
			d = addPodDemand(d, n)
		}
	}
	// Sidecars stay resident. Summing all init requests is conservative for
	// both sequential init and restartable init; never subtract their requests.
	var sidecars, init, restartable PodDemand
	sidecarContainers := append([]wfv1.UserContainer{}, t.Sidecars...)
	initContainers := append([]wfv1.UserContainer{}, t.InitContainers...)
	if defaults != nil {
		sidecarContainers = append(sidecarContainers, defaults.Sidecars...)
		initContainers = append(initContainers, defaults.InitContainers...)
	}
	for _, c := range sidecarContainers {
		n, err := containerDemand(&c.Container, nil)
		if err != nil {
			return d, err
		}
		sidecars = addPodDemand(sidecars, n)
	}
	for _, c := range initContainers {
		n, err := containerDemand(&c.Container, nil)
		if err != nil {
			return d, err
		}
		init = addPodDemand(init, n)
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			restartable = addPodDemand(restartable, n)
		}
	}
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
		return start, start, err
	}
	end, err := g.node(PodDemand{})
	if err != nil {
		return 0, 0, err
	}
	call := func(name string, inline *wfv1.Template, ref *wfv1.TemplateRef, exit string, hooks wfv1.LifecycleHooks, copies int) (int, int, error) {
		if ref != nil || exit != "" || len(hooks) != 0 || copies < 0 {
			return 0, 0, errPodGraph
		}
		child := g.templates[name]
		if inline != nil {
			child = inline
		}
		if copies == 1 {
			return g.expand(child, depth+1)
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
			x, y, e := g.expand(child, depth+1)
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
				// OR/early-start expressions earn no ordering credits. Assuming
				// all their children run concurrently is a safe upper bound.
				deps[task.Name] = nil
				parts := strings.Split(task.Depends, "&&")
				valid := true
				for _, part := range parts {
					valid = valid && successfulDependency.MatchString(strings.TrimSpace(part))
				}
				if valid {
					for _, part := range parts {
						deps[task.Name] = append(deps[task.Name], strings.TrimSuffix(strings.TrimSpace(part), ".Succeeded"))
					}
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
	return start, end, nil
}

// DeclaredPodPeak returns nil for incomplete declarations. Fully declared but
// dynamic/unsupported graphs require a workflow parallelism bound; that path
// reserves the largest possible pod in each dimension times the bound. It does
// not infer fanout size or condition results from mutable inputs.
func DeclaredPodPeak(wf *wfv1.Workflow) (*PodDemand, error) { return declaredPodPeak(wf, false) }

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
	g := podGraph{resourcePods: resourcePods, templates: map[string]*wfv1.Template{}, defaults: wf.Spec.TemplateDefaults, stack: map[*wfv1.Template]bool{}}
	for i := range wf.Spec.Templates {
		t := &wf.Spec.Templates[i]
		g.templates[t.Name] = t
	}
	_, _, err := g.expand(g.templates[wf.Spec.Entrypoint], 0)
	if errors.Is(err, errPodUndeclared) {
		return nil, nil
	}
	if wf.Spec.OnExit != "" || len(wf.Spec.Hooks) != 0 {
		return nil, errPodGraph
	}
	if err != nil {
		return boundedPodPeak(wf, resourcePods)
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
	cpu, memory, pods := make([]int64, n), make([]int64, n), make([]int64, n)
	for i, d := range g.demands {
		if reach[i][i] {
			return nil, errPodGraph
		}
		cpu[i], memory[i], pods[i] = d.CPUMilli, d.MemoryBytes, d.Pods
	}
	d := PodDemand{CPUMilli: maxWeightedVMAntichain(reach, cpu), MemoryBytes: maxWeightedVMAntichain(reach, memory), Pods: maxWeightedVMAntichain(reach, pods)}
	if p := wf.Spec.Parallelism; p != nil && *p > 0 && *p < d.Pods {
		d.CPUMilli = min(d.CPUMilli, largestPodRequests(cpu, int(*p)))
		d.MemoryBytes = min(d.MemoryBytes, largestPodRequests(memory, int(*p)))
		d.Pods = *p
	}
	emptyExecutor := resourcePods && d.Pods > 0 && d.CPUMilli == 0 && d.MemoryBytes == 0
	if !d.Valid() && !emptyExecutor {
		return nil, errPodGraph
	}
	return &d, nil
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
		if t.Container != nil || t.Script != nil || t.ContainerSet != nil {
			d, err := templatePodDemand(t, wf.Spec.TemplateDefaults, resourcePods)
			if err != nil {
				return err
			}
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
	d := PodDemand{CPUMilli: maximum.CPUMilli * *p, MemoryBytes: maximum.MemoryBytes * *p, Pods: *p}
	if !d.Valid() {
		return nil, fmt.Errorf("%w: requests exceed graph bounds", errPodGraph)
	}
	return &d, nil
}

func podFanoutCount(items []wfv1.Item, param string, sequence *wfv1.Sequence) int {
	if sequence != nil {
		return -1
	}
	n := 1
	if items != nil {
		n = len(items)
	}
	if param != "" {
		var values []json.RawMessage
		if json.Unmarshal([]byte(param), &values) != nil || !strings.HasPrefix(strings.TrimSpace(param), "[") {
			return -1
		}
		n = len(values)
	}
	if n > 128 {
		return -1
	}
	return n
}
