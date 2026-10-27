package argoworkflow

import (
	"context"
	"strings"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/workflow/common"
	"github.com/expr-lang/expr"
)

// Match Argo v4.0.8 controller.TaskResults without importing the controller.
// The compiler must see bool fields, not an untyped map or predicted statuses.
type vmiArgoTaskResults struct {
	Succeeded, Failed, Errored, Skipped, Omitted, Daemoned, AnySucceeded, AllFailed bool
}

type vmiArgoDAGContext map[string]*wfv1.DAGTask

func (d vmiArgoDAGContext) GetTask(_ context.Context, name string) *wfv1.DAGTask {
	return d[name]
}

// GetTaskDependencies only needs GetTask. These methods complete its pinned
// DagContext interface; ancestry/timestamps are not used for expression expansion.
func (vmiArgoDAGContext) GetTaskDependencies(context.Context, string) []string { return nil }
func (vmiArgoDAGContext) GetTaskFinishedAtTime(context.Context, string) time.Time {
	return time.Time{}
}

// Called only AFTER closed-grammar parsing, reference resolution and cycle
// checks, so Argo's bare/legacy expander never receives a missing predecessor.
// Use its exact expansion, hyphen normalization and typed compiler environment.
// Compilation executes no repository expression or planner. A compile error
// makes the lifetime proof unsupported: Argo can mark the dependent Errored
// before its gate succeeds, invalidating a three-state execution prerequisite.
func compileVMIArgoDependencies(dag *wfv1.DAGTemplate) bool {
	tasks := vmiArgoDAGContext{}
	for i := range dag.Tasks {
		task := &dag.Tasks[i]
		if !vmiDNSLabelPattern.MatchString(task.Name) {
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
		deps, logic := common.GetTaskDependencies(context.Background(), &dag.Tasks[i], tasks)
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
