package argoworkflow

import wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

func (g *podGraph) expandWorkflow(wf *wfv1.Workflow) error {
	_, end, err := g.expand(g.templates[wf.Spec.Entrypoint], 0)
	if err != nil {
		return err
	}
	if wf.Spec.OnExit != "" {
		first, _, err := g.expand(g.templates[wf.Spec.OnExit], 0)
		if err != nil {
			return err
		}
		g.edges[end] = append(g.edges[end], first)
	}
	// Workflow lifecycle hooks may overlap its main graph. Expressions do not
	// earn concurrency reductions; scan every referenced hook template.
	for _, hook := range wf.Spec.Hooks {
		if hook.TemplateRef != nil {
			return errPodGraph
		}
		if _, _, err := g.expand(g.templates[hook.Template], 0); err != nil {
			return err
		}
	}
	return nil
}

func scanHooks(exit string, hooks wfv1.LifecycleHooks, templates map[string]*wfv1.Template, scan func(*wfv1.Template, int) error, depth int) error {
	if exit != "" {
		if err := scan(templates[exit], depth); err != nil {
			return err
		}
	}
	for _, hook := range hooks {
		if hook.TemplateRef != nil {
			return errPodGraph
		}
		if err := scan(templates[hook.Template], depth); err != nil {
			return err
		}
	}
	return nil
}

func (g *podGraph) expandCall(name string, inline *wfv1.Template, ref *wfv1.TemplateRef, exit string, hooks wfv1.LifecycleHooks, copies int, fanoutLimit, localLimit int64, depth int, children *[]PodDemand) (int, int, error) {
	if ref != nil {
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
	expand := func() (int, int, error) {
		first := len(g.edges)
		a, b, err := g.expand(child, depth+1)
		if err != nil {
			return 0, 0, err
		}
		if exit != "" {
			x, y, err := g.expand(g.templates[exit], depth+1)
			if err != nil {
				return 0, 0, err
			}
			g.edges[b] = append(g.edges[b], x)
			b = y
		}
		if len(hooks) > 0 {
			start, err := g.node(PodDemand{})
			if err != nil {
				return 0, 0, err
			}
			end, err := g.node(PodDemand{})
			if err != nil {
				return 0, 0, err
			}
			g.edges[start] = append(g.edges[start], a)
			g.edges[b] = append(g.edges[b], end)
			for _, hook := range hooks {
				if hook.TemplateRef != nil {
					return 0, 0, errPodGraph
				}
				x, y, err := g.expand(g.templates[hook.Template], depth+1)
				if err != nil {
					return 0, 0, err
				}
				g.edges[start] = append(g.edges[start], x)
				g.edges[y] = append(g.edges[y], end)
			}
			a, b = start, end
		}
		if localLimit > 0 {
			d, err := g.peak(first, len(g.edges))
			if err != nil {
				return 0, 0, err
			}
			*children = append(*children, d)
			if child != nil && (child.DAG != nil || len(child.Steps) > 0) {
				g.assumptions |= PodPeakNested
			}
		}
		return a, b, nil
	}
	if copies == 1 {
		return expand()
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
		x, y, err := expand()
		if err != nil {
			return 0, 0, err
		}
		g.edges[a] = append(g.edges[a], x)
		g.edges[y] = append(g.edges[y], b)
	}
	return a, b, nil
}
