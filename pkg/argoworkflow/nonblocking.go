package argoworkflow

import (
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

// NonBlockingTasks returns the set of DAG task names in the Workflow's
// entrypoint DAG whose failure the pipeline author explicitly declared
// non-blocking through status-function depends expressions.
//
// A task is non-blocking when every reference to it in any depends expression
// uses explicit status functions that include .Failed or .Errored, and it
// never appears as a bare name or with .Succeeded alone (both of which Argo
// treats as requiring the task to succeed). The pipeline author designed the
// DAG so that no downstream task is blocked by this task's failure.
//
// The analysis is limited to the entrypoint DAG because Oberth's burn/step
// model maps entrypoint-level DAG task names to burn names: a failed step's
// burn identifies the entrypoint task it belongs to, which is where the
// non-blocking decision is made.
//
// A task that appears in no depends expression (a leaf task, or a root with
// no dependents) is never non-blocking: its failure is a real pipeline
// failure. Old-style Dependencies arrays (the predecessor of depends
// expressions) always imply .Succeeded and make the referenced task blocking.
func NonBlockingTasks(workflow *wfv1.Workflow) map[string]bool {
	if workflow == nil {
		return nil
	}
	entrypoint := strings.TrimSpace(workflow.Spec.Entrypoint)
	if entrypoint == "" {
		return nil
	}
	var dag *wfv1.DAGTemplate
	for i := range workflow.Spec.Templates {
		if workflow.Spec.Templates[i].Name == entrypoint {
			dag = workflow.Spec.Templates[i].DAG
			break
		}
	}
	if dag == nil {
		return nil
	}

	taskNames := make(map[string]bool, len(dag.Tasks))
	for _, task := range dag.Tasks {
		taskNames[task.Name] = true
	}

	type referenceInfo struct {
		referenced bool // appears in at least one depends expression
		blocked    bool // at least one expression requires this task to succeed
	}
	info := make(map[string]*referenceInfo, len(dag.Tasks))
	for name := range taskNames {
		info[name] = &referenceInfo{}
	}

	for _, task := range dag.Tasks {
		// Old-style dependencies array: every listed task must succeed.
		for _, dep := range task.Dependencies {
			dep = strings.TrimSpace(dep)
			if dep == "" || !taskNames[dep] {
				continue
			}
			ri := info[dep]
			ri.referenced = true
			ri.blocked = true
		}

		depends := strings.TrimSpace(task.Depends)
		if depends == "" {
			continue
		}
		refs := parseDependsReferences(depends)
		for refName, statuses := range refs {
			if !taskNames[refName] {
				continue
			}
			ri := info[refName]
			ri.referenced = true

			if statuses.bare {
				// A bare reference (just the task name) implies .Succeeded:
				// the expression requires this task to succeed.
				ri.blocked = true
				continue
			}
			// Explicit status functions without .Failed or .Errored mean
			// the expression does not handle this task's failure.
			if !statuses.failed && !statuses.errored {
				ri.blocked = true
			}
		}
	}

	result := make(map[string]bool)
	for name, ri := range info {
		if ri.referenced && !ri.blocked {
			result[name] = true
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// dependsStatuses tracks which reference forms are used for a single task
// within one depends expression.
type dependsStatuses struct {
	bare      bool // the task name appears without a status function
	succeeded bool
	failed    bool
	errored   bool
	skipped   bool
	daemoned  bool
}

// parseDependsReferences extracts all task-name references from an Argo
// depends expression. A bare reference (just the task name, no .Status
// suffix) is recorded separately from status-function references.
//
// The parser is deliberately simple: it replaces boolean operators and
// grouping characters with whitespace, then examines each resulting token.
// This is sufficient because Argo's depends grammar uses only these
// separators between task references, and a task name is always a
// DNS-1123 label (alphanumeric and hyphens, no spaces or operators).
func parseDependsReferences(depends string) map[string]*dependsStatuses {
	result := make(map[string]*dependsStatuses)
	cleaned := replaceDependsOperators(depends)
	for _, token := range strings.Fields(cleaned) {
		if token == "" {
			continue
		}
		dot := strings.LastIndex(token, ".")
		if dot > 0 && dot < len(token)-1 {
			task := token[:dot]
			status := token[dot+1:]
			if applyStatusFunction(result, task, status) {
				continue
			}
		}
		// Bare reference: no recognized status function suffix.
		entry := result[token]
		if entry == nil {
			entry = &dependsStatuses{}
			result[token] = entry
		}
		entry.bare = true
	}
	return result
}

func applyStatusFunction(refs map[string]*dependsStatuses, task, status string) bool {
	entry := refs[task]
	switch status {
	case "Succeeded":
		if entry == nil {
			entry = &dependsStatuses{}
			refs[task] = entry
		}
		entry.succeeded = true
	case "Failed":
		if entry == nil {
			entry = &dependsStatuses{}
			refs[task] = entry
		}
		entry.failed = true
	case "Errored":
		if entry == nil {
			entry = &dependsStatuses{}
			refs[task] = entry
		}
		entry.errored = true
	case "Skipped":
		if entry == nil {
			entry = &dependsStatuses{}
			refs[task] = entry
		}
		entry.skipped = true
	case "Daemoned":
		if entry == nil {
			entry = &dependsStatuses{}
			refs[task] = entry
		}
		entry.daemoned = true
	default:
		return false
	}
	return true
}

// replaceDependsOperators replaces all boolean operators and grouping
// characters in a depends expression with spaces. The result can be split
// on whitespace to extract individual task references.
func replaceDependsOperators(depends string) string {
	// Pre-size for the common case where the string length does not grow.
	out := make([]byte, 0, len(depends))
	for i := 0; i < len(depends); i++ {
		switch depends[i] {
		case '(', ')', '!':
			out = append(out, ' ')
		case '&':
			out = append(out, ' ')
			if i+1 < len(depends) && depends[i+1] == '&' {
				i++
			}
		case '|':
			out = append(out, ' ')
			if i+1 < len(depends) && depends[i+1] == '|' {
				i++
			}
		default:
			out = append(out, depends[i])
		}
	}
	return string(out)
}
