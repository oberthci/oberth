package argoworkflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/pkg/periapsis"
)

// RunTrigger is what started a durable run, finer than the trust-tier
// periapsis.Trigger: build.yaml runs for a branch push, a promotion candidate,
// or a schedule, and release.yaml for a tag. The vocabulary is closed. The
// server derives the value from the durable run record, never from the
// document or a caller.
type RunTrigger string

const (
	RunTriggerBranch    RunTrigger = "branch"
	RunTriggerPromotion RunTrigger = "promotion"
	RunTriggerSchedule  RunTrigger = "schedule"
	RunTriggerTag       RunTrigger = "tag"
)

// RunTriggers lists the closed vocabulary in documentation order.
func RunTriggers() []RunTrigger {
	return []RunTrigger{RunTriggerBranch, RunTriggerPromotion, RunTriggerSchedule, RunTriggerTag}
}

// Valid reports whether the value is one of the closed run triggers.
func (trigger RunTrigger) Valid() bool {
	switch trigger {
	case RunTriggerBranch, RunTriggerPromotion, RunTriggerSchedule, RunTriggerTag:
		return true
	default:
		return false
	}
}

// Tier returns the trust tier, and so the pipeline document, a run trigger
// executes under: tag runs read release.yaml, every other run build.yaml.
func (trigger RunTrigger) Tier() periapsis.Trigger {
	if trigger == RunTriggerTag {
		return periapsis.TriggerRelease
	}
	return periapsis.TriggerCI
}

const (
	// SizeByTriggerAnnotation declares the admission weight per run trigger,
	// as comma-separated trigger=size pairs:
	//
	//	oberth.ci/size-by-trigger: "branch=M,promotion=XL,schedule=XL"
	//
	// Triggers are branch, promotion, and schedule in build.yaml and tag in
	// release.yaml; sizes are S, M, L, and XL. A trigger the annotation does
	// not name falls back to oberth.ci/size (and that to M). Like
	// oberth.ci/size it is read statically from the run's own immutable
	// document before admission; an unknown trigger or size, a duplicate, or a
	// trigger the document can never run for refuses the run.
	//
	// It exists so a repository whose full gate is heavy (a kernel matrix, say)
	// can keep that gate on promotion and nightly runs without making every
	// branch push drain the host. Pair it with a `when:` guard over the
	// oberth-trigger workflow parameter so the skipped work actually skips.
	SizeByTriggerAnnotation = "oberth.ci/size-by-trigger"

	// TriggerParameter is the server-owned workflow parameter carrying the
	// run's RunTrigger, readable in templates and `when:` guards as
	// {{workflow.parameters.oberth-trigger}}. The server sets it from the
	// durable run on every document that declares or mentions it (and leaves
	// every other document untouched). A document may declare it in
	// spec.arguments with a plain vocabulary value; that value is only a
	// default for servers that predate the parameter, and this server
	// replaces it.
	TriggerParameter = "oberth-trigger"
)

// DeclaredSizesByTrigger parses the size-by-trigger annotation. An absent or
// blank annotation yields nil.
func DeclaredSizesByTrigger(workflow *wfv1.Workflow) (map[RunTrigger]periapsis.Size, error) {
	if workflow == nil {
		return nil, errors.New("argoworkflow: no Workflow to read")
	}
	declared, ok := workflow.Annotations[SizeByTriggerAnnotation]
	if !ok || strings.TrimSpace(declared) == "" {
		return nil, nil
	}
	sizes := map[RunTrigger]periapsis.Size{}
	for index, entry := range strings.Split(declared, ",") {
		key, value, found := strings.Cut(entry, "=")
		trigger := RunTrigger(strings.TrimSpace(key))
		size := periapsis.Size(strings.TrimSpace(value))
		if !found || trigger == "" || size == "" {
			return nil, fmt.Errorf("argoworkflow: annotation %s entry %d %q is not trigger=size",
				SizeByTriggerAnnotation, index, strings.TrimSpace(entry))
		}
		if !trigger.Valid() {
			return nil, fmt.Errorf("argoworkflow: annotation %s declares unknown trigger %q; triggers are %s",
				SizeByTriggerAnnotation, trigger, quotedTriggers())
		}
		switch size {
		case periapsis.S, periapsis.M, periapsis.L, periapsis.XL:
		default:
			return nil, fmt.Errorf("argoworkflow: annotation %s declares unknown size %q for %s; sizes are %q, %q, %q, and %q",
				SizeByTriggerAnnotation, size, trigger, periapsis.S, periapsis.M, periapsis.L, periapsis.XL)
		}
		if _, duplicate := sizes[trigger]; duplicate {
			return nil, fmt.Errorf("argoworkflow: annotation %s declares %s more than once", SizeByTriggerAnnotation, trigger)
		}
		sizes[trigger] = size
	}
	return sizes, nil
}

// AdmitSizeDeclarations checks both size annotations against the document
// tier: every size and trigger is from its closed vocabulary, and every
// trigger named is one this document can run for (tag only in release.yaml,
// the others only in build.yaml). It is the check `oberth validate` and the
// server share.
func AdmitSizeDeclarations(workflow *wfv1.Workflow, tier periapsis.Trigger) error {
	if _, err := DeclaredSize(workflow); err != nil {
		return err
	}
	sizes, err := DeclaredSizesByTrigger(workflow)
	if err != nil {
		return err
	}
	file, err := TriggerFile(tier)
	if err != nil {
		return err
	}
	for _, trigger := range RunTriggers() {
		if _, named := sizes[trigger]; named && trigger.Tier() != tier {
			return fmt.Errorf("argoworkflow: annotation %s names %s, but %s never runs for a %s run",
				SizeByTriggerAnnotation, trigger, file, trigger)
		}
	}
	return nil
}

// DeclaredSizeFor returns the admission weight the document requests for one
// run trigger: its size-by-trigger entry when present, otherwise
// oberth.ci/size, otherwise M.
func DeclaredSizeFor(workflow *wfv1.Workflow, trigger RunTrigger) (periapsis.Size, error) {
	if !trigger.Valid() {
		return "", fmt.Errorf("argoworkflow: run trigger %q is invalid", trigger)
	}
	if err := AdmitSizeDeclarations(workflow, trigger.Tier()); err != nil {
		return "", err
	}
	sizes, err := DeclaredSizesByTrigger(workflow)
	if err != nil {
		return "", err
	}
	if size, named := sizes[trigger]; named {
		return size, nil
	}
	return DeclaredSize(workflow)
}

// admitTriggerParameter refuses a document-declared oberth-trigger parameter
// that is anything but a plain default from the closed vocabulary. The server
// replaces the value either way; refusing other shapes keeps a reader of the
// document from mistaking a valueFrom or enum for something that steers it.
func admitTriggerParameter(spec *wfv1.WorkflowSpec) []error {
	var problems []error
	seen := false
	for _, parameter := range spec.Arguments.Parameters {
		if parameter.Name != TriggerParameter {
			continue
		}
		if seen {
			problems = append(problems, fmt.Errorf("argoworkflow: spec.arguments.parameters declares %s more than once", TriggerParameter))
			continue
		}
		seen = true
		value := ""
		if parameter.Value != nil {
			value = parameter.Value.String()
		}
		if parameter.ValueFrom != nil || parameter.Default != nil || len(parameter.Enum) != 0 || !RunTrigger(value).Valid() {
			problems = append(problems, fmt.Errorf("argoworkflow: spec.arguments.parameters[%q] is server-owned; "+
				"a document may only declare it with a plain value from %s as the default for older servers",
				TriggerParameter, quotedTriggers()))
		}
	}
	return problems
}

// UsesTriggerParameter reports whether the document declares or mentions the
// oberth-trigger parameter anywhere in its spec. Only such a document is
// stamped, so every other pipeline's submitted Workflow stays byte-identical
// to what it was before the parameter existed. Over-matching (the name inside
// some unrelated string) only stamps a parameter nothing reads.
func UsesTriggerParameter(workflow *wfv1.Workflow) (bool, error) {
	if workflow == nil {
		return false, errors.New("argoworkflow: no Workflow to read")
	}
	encoded, err := json.Marshal(workflow.Spec)
	if err != nil {
		return false, fmt.Errorf("argoworkflow: encode Workflow spec: %w", err)
	}
	return bytes.Contains(encoded, []byte(TriggerParameter)), nil
}

// SetTriggerParameter writes the server-owned oberth-trigger workflow
// parameter when the document uses it, replacing any document-declared
// default in place.
func SetTriggerParameter(workflow *wfv1.Workflow, trigger RunTrigger) error {
	if !trigger.Valid() {
		return fmt.Errorf("argoworkflow: run trigger %q is invalid", trigger)
	}
	used, err := UsesTriggerParameter(workflow)
	if err != nil || !used {
		return err
	}
	parameter := wfv1.Parameter{Name: TriggerParameter, Value: wfv1.AnyStringPtr(string(trigger))}
	kept := workflow.Spec.Arguments.Parameters[:0:0]
	replaced := false
	for _, existing := range workflow.Spec.Arguments.Parameters {
		if existing.Name != TriggerParameter {
			kept = append(kept, existing)
			continue
		}
		if !replaced {
			kept = append(kept, parameter)
			replaced = true
		}
	}
	if !replaced {
		kept = append(kept, parameter)
	}
	workflow.Spec.Arguments.Parameters = kept
	return nil
}

func quotedTriggers() string {
	names := make([]string, 0, len(RunTriggers()))
	for _, trigger := range RunTriggers() {
		names = append(names, fmt.Sprintf("%q", trigger))
	}
	return strings.Join(names, ", ")
}
