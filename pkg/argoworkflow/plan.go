package argoworkflow

import (
	"errors"
	"fmt"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

// PlannedStep is one step the reviewed document declares, named in Oberth's
// burn/step vocabulary and numbered in declaration order.
//
// Ordinal is the plan's own ordering, not an execution order: a DAG runs its
// tasks in dependency order and a repository is free to declare them in any
// order it likes. The plan ordinal is the stable one, so a run's step list
// reads the same before it starts, while it runs, and after it finishes.
type PlannedStep struct {
	Burn    string
	Step    string
	Ordinal int
}

// MaxPlannedSteps bounds the enumeration. A repository-authored document is
// untrusted input, and admission bounds the template count but not the number
// of pods a nest of DAGs can express, so the walk carries its own ceiling
// rather than trusting the product of the two.
const MaxPlannedSteps = 512

// PlannedSteps enumerates every step the document declares, in declaration
// order, from the already-decoded object.
//
// It reads and never evaluates, exactly like DeclaredSize and
// DeclaredSecretPaths: the plan is a statement about the reviewed bytes, not a
// prediction about the run. A planned step may legitimately never execute --
// a `when` guard, an unsatisfied `depends`, or a failure upstream of it -- and
// that is the point. The states that make a declared-but-unreached step
// visible are what this exists to enable; today such a step is not "pending",
// it is absent.
//
// The burn/step derivation is the same rule the runtime applies to Argo's node
// tree (see BurnAndStep). Both sides call one function so a document cannot
// plan one set of names and record another.
//
// Two shapes are deliberately not enumerated:
//
//   - A task or step carrying withItems, withParam or withSequence expands
//     into N pods whose count is a runtime property of the parameter. One
//     planned row for the task would be a row no observation could ever
//     reconcile, so such an invocation is left out and behaves exactly as it
//     does today: visible once it runs. Fabricating a count would be worse
//     than the gap.
//   - An entrypoint that is itself a pod-producing template. Argo names that
//     single node after the Workflow object, which the document does not know,
//     so the plan would name a step the run never reports.
func PlannedSteps(workflow *wfv1.Workflow) ([]PlannedStep, error) {
	if workflow == nil {
		return nil, errors.New("argoworkflow: no Workflow to enumerate")
	}
	entrypoint := strings.TrimSpace(workflow.Spec.Entrypoint)
	if entrypoint == "" {
		return nil, errors.New("argoworkflow: spec.entrypoint is required to enumerate steps")
	}
	templates := make(map[string]*wfv1.Template, len(workflow.Spec.Templates))
	for index := range workflow.Spec.Templates {
		template := &workflow.Spec.Templates[index]
		// Admission refuses duplicate template names; on the unadmitted path
		// the first declaration wins, which is what Argo's own resolver does.
		if _, exists := templates[template.Name]; !exists {
			templates[template.Name] = template
		}
	}
	root, ok := templates[entrypoint]
	if !ok {
		return nil, fmt.Errorf("argoworkflow: spec.entrypoint %q names no template", entrypoint)
	}
	walk := &planWalk{templates: templates, onPath: map[string]struct{}{entrypoint: {}}}
	// The entrypoint is the root boundary. A pod it invokes directly is a burn
	// of one step, which is exactly what the runtime derives from a pod node
	// whose BoundaryID is the root node.
	if err := walk.descend(root, "", 0); err != nil {
		return nil, err
	}
	// Reject duplicate normalized (burn, step) keys. Two branches invoking
	// the same nested DAG can normalize to the same key; the durable result
	// model cannot represent them (deduplicateRetries collapses duplicates),
	// so one branch's result and log would disappear.
	seen := make(map[[2]string]struct{}, len(walk.steps))
	for _, step := range walk.steps {
		key := [2]string{step.Burn, step.Step}
		if _, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf("argoworkflow: duplicate step key (%s, %s) — "+
				"two pipeline branches invoke templates that normalize to the same burn/step name; "+
				"rename one of the invoking tasks to disambiguate",
				step.Burn, step.Step)
		}
		seen[key] = struct{}{}
	}
	return walk.steps, nil
}

// planWalk carries the accumulator across the same recursive descent
// applyServerSecurity and injectSourceCheckout already make over this
// structure: templates, step groups, and DAG tasks, with a depth bound.
type planWalk struct {
	templates map[string]*wfv1.Template
	onPath    map[string]struct{}
	steps     []PlannedStep
}

func (walk *planWalk) descend(template *wfv1.Template, enclosing string, depth int) error {
	if depth > MaxIdentityWalkDepth {
		return fmt.Errorf("argoworkflow: pipeline nests templates deeper than %d levels", MaxIdentityWalkDepth)
	}
	for _, called := range invocations(template) {
		if called.expands {
			continue
		}
		target := walk.resolve(called)
		if target == nil {
			// Admission refuses an unresolvable by-name reference. Reaching
			// here means the caller enumerated an unadmitted document; skip
			// rather than invent a step.
			continue
		}
		switch {
		case target.DAG != nil || len(target.Steps) != 0:
			// A nested DAG or steps template is a boundary: every pod inside
			// it reports this invocation's name as its burn, however deep the
			// nesting goes, because Argo sets a pod's BoundaryID to the
			// nearest enclosing template invocation.
			if called.template != "" {
				if _, cycle := walk.onPath[called.template]; cycle {
					return fmt.Errorf("argoworkflow: template %q is reachable from itself", called.template)
				}
				walk.onPath[called.template] = struct{}{}
			}
			err := walk.descend(target, called.name, depth+1)
			if called.template != "" {
				delete(walk.onPath, called.template)
			}
			if err != nil {
				return err
			}
		case podProducing(target):
			burn, step := BurnAndStep(enclosing, called.name)
			if burn == "" || step == "" {
				continue
			}
			if len(walk.steps) >= MaxPlannedSteps {
				return fmt.Errorf("argoworkflow: pipeline declares more than %d steps", MaxPlannedSteps)
			}
			walk.steps = append(walk.steps, PlannedStep{Burn: burn, Step: step, Ordinal: len(walk.steps)})
		}
	}
	return nil
}

func (walk *planWalk) resolve(called invocation) *wfv1.Template {
	if called.inline != nil {
		return called.inline
	}
	if called.template == "" {
		return nil
	}
	return walk.templates[called.template]
}

// invocation is one call site inside a DAG or steps template, normalised so
// both shapes walk identically.
type invocation struct {
	name     string
	template string
	inline   *wfv1.Template
	expands  bool
}

func invocations(template *wfv1.Template) []invocation {
	if template == nil {
		return nil
	}
	var called []invocation
	if template.DAG != nil {
		for index := range template.DAG.Tasks {
			task := &template.DAG.Tasks[index]
			called = append(called, invocation{
				name: task.Name, template: task.Template, inline: task.Inline,
				expands: len(task.WithItems) != 0 || strings.TrimSpace(task.WithParam) != "" || task.WithSequence != nil,
			})
		}
	}
	for group := range template.Steps {
		for index := range template.Steps[group].Steps {
			step := &template.Steps[group].Steps[index]
			called = append(called, invocation{
				name: step.Name, template: step.Template, inline: step.Inline,
				expands: len(step.WithItems) != 0 || strings.TrimSpace(step.WithParam) != "" || step.WithSequence != nil,
			})
		}
	}
	return called
}

// podProducing reports whether a template runs a Pod, which is what a step is.
// Every other executable template kind (resource, plugin, http, data, suspend)
// is refused by Admit, so this list is the complete set a submitted document
// can contain.
func podProducing(template *wfv1.Template) bool {
	return template != nil &&
		(template.Container != nil || template.Script != nil || template.ContainerSet != nil)
}

// BurnAndStep is Oberth's two-level naming rule, in one place.
//
// A pod enclosed by a nested DAG or steps invocation belongs to that
// invocation's burn. A pod invoked directly by the entrypoint is a burn of one
// step. enclosing is empty for the second case.
//
// The static enumeration passes the invocation names it reads from the
// document; the runtime passes the display names it reads from Argo's node
// tree. They agree by construction because the rule lives here rather than
// being restated on each side -- a divergence would seed one set of step rows
// and then record a second, disjoint set beside them.
func BurnAndStep(enclosing, invoked string) (string, string) {
	step := StepName(invoked)
	burn := StepName(enclosing)
	if burn == "" {
		return step, step
	}
	return burn, step
}

// StepName normalises one burn or step name. The characters removed are the
// ones the progress protocol rejects outright (NUL, CR, LF) and the two the
// run log's own "[burn/step] " line prefix is delimited by, which would
// otherwise let a name break the log index that attributes output to it.
func StepName(raw string) string {
	return strings.Map(func(character rune) rune {
		switch character {
		case '\x00', '\r', '\n', '[', ']':
			return '-'
		default:
			return character
		}
	}, strings.TrimSpace(raw))
}
