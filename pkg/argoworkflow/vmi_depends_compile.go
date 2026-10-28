package argoworkflow

import (
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/expr-lang/expr"
)

// Match Argo v4.0.8 controller.TaskResults without importing the controller.
// The compiler must see bool fields, not an untyped map or predicted statuses.
type vmiArgoTaskResults struct {
	Succeeded, Failed, Errored, Skipped, Omitted, Daemoned, AnySucceeded, AllFailed bool
}

// Called only AFTER closed-grammar parsing, reference resolution and cycle
// checks. The vendored expander also refuses nil/missing predecessors on its
// own, so a future call-order change cannot turn bad input into a panic.
// Use its exact expansion, hyphen normalization and typed compiler environment.
// Compilation executes no repository expression or planner. A compile error
// makes the lifetime proof unsupported: Argo can mark the dependent Errored
// before its gate succeeds, invalidating a three-state execution prerequisite.
func compileVMIArgoDependencies(dag *wfv1.DAGTemplate) bool {
	if dag == nil || len(dag.Tasks) > MaxDAGTasks {
		return false
	}
	tasks := map[string]*wfv1.DAGTask{}
	for i := range dag.Tasks {
		task := &dag.Tasks[i]
		if !vmiDNSLabelPattern.MatchString(task.Name) || tasks[task.Name] != nil {
			return false
		}
		tasks[task.Name] = task
		name := strings.ReplaceAll(task.Name, "-", "_")
		// DNS labels include expression keywords and leading digits. Refuse
		// names that cannot be used as Argo's result identifiers, even if a
		// particular authored graph does not reference them yet.
		if _, err := expr.Compile(name+".Succeeded", expr.Env(map[string]vmiArgoTaskResults{name: {}})); err != nil {
			return false
		}
	}
	for i := range dag.Tasks {
		task := &dag.Tasks[i]
		if len(task.Depends) > 4096 || len(task.Dependencies) > MaxDAGTasks {
			return false
		}
		deps, logic, ok := expandVMIArgoDependencies(task, tasks)
		if !ok {
			return false
		}
		if logic == "" { // Argo bypasses evaluateDependsLogic for root tasks.
			continue
		}
		env := map[string]vmiArgoTaskResults{}
		for name := range deps {
			if tasks[name] == nil {
				return false
			}
			env[strings.ReplaceAll(name, "-", "_")] = vmiArgoTaskResults{}
		}
		logic = strings.ReplaceAll(logic, "-", "_")
		if len(logic) > 4096 { // Bare/legacy expansion must remain bounded too.
			return false
		}
		if _, err := expr.Compile(logic, expr.Env(env)); err != nil {
			return false
		}
	}
	return true
}
