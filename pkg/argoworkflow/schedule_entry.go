package argoworkflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

// ScheduleParameter identifies the resolved schedule entry of the durable run.
// Repository documents may reference it, but cannot declare its value.
const ScheduleParameter = "oberth-schedule"

func ValidateScheduleEntry(trigger RunTrigger, entry string) error {
	if !trigger.Valid() || len(entry) > 64 || strings.TrimSpace(entry) != entry || (entry != "" && trigger != RunTriggerSchedule) {
		return errors.New("argoworkflow: schedule entry requires a schedule run and at most 64 bytes")
	}
	return nil
}

func admitScheduleParameter(spec *wfv1.WorkflowSpec) []error {
	for _, parameter := range spec.Arguments.Parameters {
		if parameter.Name == ScheduleParameter {
			return []error{fmt.Errorf("argoworkflow: workflow parameter %s is reserved and server-owned; reference it without declaring a default", ScheduleParameter)}
		}
	}
	return nil
}

func SetScheduleParameter(workflow *wfv1.Workflow, trigger RunTrigger, entry string) error {
	if workflow == nil {
		return errors.New("argoworkflow: no Workflow to stamp")
	}
	if err := ValidateScheduleEntry(trigger, entry); err != nil {
		return err
	}
	encoded, err := json.Marshal(workflow.Spec)
	if err != nil {
		return err
	}
	if entry == "" && !bytes.Contains(encoded, []byte(ScheduleParameter)) {
		return nil
	}
	kept := workflow.Spec.Arguments.Parameters[:0:0]
	for _, parameter := range workflow.Spec.Arguments.Parameters {
		if parameter.Name != ScheduleParameter {
			kept = append(kept, parameter)
		}
	}
	workflow.Spec.Arguments.Parameters = append(kept, wfv1.Parameter{Name: ScheduleParameter, Value: wfv1.AnyStringPtr(entry)})
	return nil
}

// ScheduleEntryMatches also admits pre-feature Workflows with no parameter
// when their durable run has unknown/empty entry identity. Never infer it.
func ScheduleEntryMatches(workflow *wfv1.Workflow, trigger RunTrigger, entry string) bool {
	if workflow == nil || ValidateScheduleEntry(trigger, entry) != nil {
		return false
	}
	found := false
	for _, p := range workflow.Spec.Arguments.Parameters {
		if p.Name != ScheduleParameter {
			continue
		}
		if found || p.Value == nil || p.ValueFrom != nil || p.Default != nil || len(p.Enum) != 0 || p.Value.String() != entry {
			return false
		}
		found = true
	}
	return found || entry == ""
}
