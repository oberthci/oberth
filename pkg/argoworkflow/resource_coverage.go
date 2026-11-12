package argoworkflow

import (
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

type ResourceDeclarationCoverage struct{ Declared, Undeclared int }

// ResourceCoverage counts reachable pod-producing call sites, once per declared
// fan-out site (not per runtime item or retry). A root pod counts too. Unknown
// graphs stay unknown; unreferenced utility templates do not inflate the count.
// Completeness means every pipeline main/container-set container explicitly
// requests positive CPU and memory, including inherited template defaults.
func ResourceCoverage(wf *wfv1.Workflow) *ResourceDeclarationCoverage {
	if wf == nil {
		return nil
	}
	templates := map[string]*wfv1.Template{}
	for i := range wf.Spec.Templates {
		t := &wf.Spec.Templates[i]
		templates[t.Name] = t
	}
	out := &ResourceDeclarationCoverage{}
	active := map[*wfv1.Template]bool{}
	visited := 0
	complete := func(c *corev1.Container, defaults *corev1.Container) bool {
		if c == nil {
			return false
		}
		for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			q, ok := c.Resources.Requests[name]
			if !ok && defaults != nil {
				q, ok = defaults.Resources.Requests[name]
			}
			if !ok || q.Sign() <= 0 {
				return false
			}
		}
		return true
	}
	var walk func(*wfv1.Template, int) bool
	walk = func(t *wfv1.Template, depth int) bool {
		if t == nil || depth > MaxIdentityWalkDepth || active[t] || visited >= MaxPlannedSteps*MaxIdentityWalkDepth {
			return false
		}
		visited++
		active[t] = true
		defer delete(active, t)
		if podProducing(t) {
			var defaults *corev1.Container
			if d := wf.Spec.TemplateDefaults; d != nil {
				if d.Container != nil {
					defaults = d.Container
				} else if d.Script != nil {
					defaults = &d.Script.Container
				}
			}
			declared := false
			switch {
			case t.Container != nil:
				declared = complete(t.Container, defaults)
			case t.Script != nil:
				declared = complete(&t.Script.Container, defaults)
			case t.ContainerSet != nil:
				declared = len(t.ContainerSet.Containers) > 0
				for i := range t.ContainerSet.Containers {
					declared = complete(&t.ContainerSet.Containers[i].Container, nil) && declared
				}
			}
			if declared {
				out.Declared++
			} else {
				out.Undeclared++
			}
			if out.Declared+out.Undeclared > MaxPlannedSteps {
				return false
			}
		}
		for _, call := range invocations(t) {
			target := call.inline
			if target == nil {
				target = templates[call.template]
			}
			if !walk(target, depth+1) {
				return false
			}
		}
		if t.DAG != nil {
			for _, task := range t.DAG.Tasks {
				for _, hook := range task.Hooks {
					if !walk(templates[hook.Template], depth+1) {
						return false
					}
				}
			}
		}
		for _, group := range t.Steps {
			for _, step := range group.Steps {
				for _, hook := range step.Hooks {
					if !walk(templates[hook.Template], depth+1) {
						return false
					}
				}
			}
		}
		return true
	}
	if !walk(templates[wf.Spec.Entrypoint], 0) {
		return nil
	}
	if wf.Spec.OnExit != "" && !walk(templates[wf.Spec.OnExit], 0) {
		return nil
	}
	for _, hook := range wf.Spec.Hooks {
		if !walk(templates[hook.Template], 0) {
			return nil
		}
	}
	return out
}
