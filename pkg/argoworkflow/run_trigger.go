package argoworkflow

import (
	"errors"
	"fmt"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/pkg/pipeline"
)

// RunTrigger is what started a durable run, finer than the trust-tier
// pipeline.Trigger: build.yaml runs for a branch push, a promotion candidate,
// a schedule, or an ordinary tag; release tags run release.yaml. The vocabulary is closed. The
// server derives the value from the durable run record, never from the
// document or a caller.
type RunTrigger string

const (
	RunTriggerBranch    RunTrigger = "branch"
	RunTriggerPromotion RunTrigger = "promotion"
	RunTriggerSchedule  RunTrigger = "schedule"
	RunTriggerTag       RunTrigger = "tag"
)

// Valid reports whether the value is one of the closed run triggers.
func (trigger RunTrigger) Valid() bool {
	switch trigger {
	case RunTriggerBranch, RunTriggerPromotion, RunTriggerSchedule, RunTriggerTag:
		return true
	default:
		return false
	}
}

// AllowsTier keeps event identity separate from release authority. A tag may
// run ordinary CI or an explicitly admitted release; other events are CI only.
func (trigger RunTrigger) AllowsTier(tier pipeline.Trigger) bool {
	if !trigger.Valid() || !tier.Valid() {
		return false
	}
	return tier == pipeline.TriggerCI || trigger == RunTriggerTag
}

// TriggerParameter is the server-owned durable run trigger, available on every
// compiled Workflow as {{workflow.parameters.oberth-trigger}}.
const TriggerParameter = "oberth-trigger"

// SetTriggerParameter writes the server-owned durable run trigger on every
// Workflow. Admission has already refused authored oberth- parameter names.
func SetTriggerParameter(workflow *wfv1.Workflow, trigger RunTrigger) error {
	if !trigger.Valid() {
		return fmt.Errorf("argoworkflow: run trigger %q is invalid", trigger)
	}
	if workflow == nil {
		return errors.New("argoworkflow: no Workflow to stamp")
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
