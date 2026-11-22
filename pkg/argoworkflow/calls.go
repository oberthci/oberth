package argoworkflow

import (
	"fmt"
	"strings"
	"unicode"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func (g *graph) templateCalls(t *wfv1.Template, where string) []*call {
	var calls []*call
	for group, steps := range t.Steps {
		for _, s := range steps.Steps {
			path := fmt.Sprintf("%s.steps[%d][%q]", where, group, s.Name)
			if !g.constructing(path) {
				return calls
			}
			calls = append(calls, &call{
				suffix: fmt.Sprintf("[%d].%s", group, s.Name), name: s.Name, path: path,
				template: s.Template, ref: s.TemplateRef, inline: s.Inline, arguments: s.Arguments,
				items: s.WithItems, dynamic: s.WithParam != "" || s.WithSequence != nil,
				hooks: g.selectedHooks(s.Hooks, s.GetExitHook(wfv1.Arguments{}), s.OnExit, path),
			})
		}
	}
	if t.DAG != nil {
		calls = append(calls, g.dagCalls(t.DAG, where)...)
	}
	return calls
}
func (g *graph) dagCalls(dag *wfv1.DAGTemplate, where string) []*call {
	var calls []*call
	for _, s := range dag.Tasks {
		path := where + ".dag.tasks[" + s.Name + "]"
		if !g.constructing(path) {
			return calls
		}
		calls = append(calls, &call{
			suffix: s.Name, name: s.Name, path: path, dag: true, after: append(dependsNames(s.Depends), s.Dependencies...),
			template: s.Template, ref: s.TemplateRef, inline: s.Inline, arguments: s.Arguments,
			items: s.WithItems, dynamic: s.WithParam != "" || s.WithSequence != nil,
			hooks: g.selectedHooks(s.Hooks, s.GetExitHook(wfv1.Arguments{}), s.OnExit, path),
		})
	}
	return calls
}

func (g *graph) selectedHooks(hooks wfv1.LifecycleHooks, exit *wfv1.LifecycleHook, onExit, where string) []*call {
	var calls []*call
	if exit != nil {
		if !g.constructing(where + ".onExit") {
			return calls
		}
		path := where + ".hooks[exit]"
		if onExit != "" {
			path = where + ".onExit"
		}
		calls = append(calls, &call{suffix: "onExit", name: "onExit", path: path, template: exit.Template, ref: exit.TemplateRef, arguments: exit.Arguments})
	}
	for _, key := range sortedKeys(hooks) {
		if !g.constructing(where + ".hooks[" + string(key) + "]") {
			return calls
		}
		h := hooks[key]
		if key == wfv1.ExitLifecycleEvent && onExit == "" {
			continue
		}
		suffix := "hooks." + string(key)
		if key == wfv1.ExitLifecycleEvent {
			suffix = "onExit"
		}
		calls = append(calls, &call{suffix: suffix, name: string(key), path: where + ".hooks[" + string(key) + "]", template: h.Template, ref: h.TemplateRef, arguments: h.Arguments, inspectionOnly: key == wfv1.ExitLifecycleEvent && onExit != ""})
	}
	return calls
}

// dependsNames lists the words of a depends expression; task results such as
// build.Succeeded split into the task name and the result, and the planner keeps
// only words that name a task of the same DAG.
func dependsNames(expression string) []string {
	return strings.FieldsFunc(expression, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_'
	})
}
