package service

import (
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// Only typed references captured from this admitted Workflow binding survive
// the public boundary. Missing provenance keeps the cause, omitting identities.
func bindNativeReferences(r *stateport.Run) {
	bindNativeFailure(r)
	for i := range r.Steps {
		step := &r.Steps[i]
		if ownNativeStep(r, *step) {
			continue
		}
		step.Logs = nil
		for j := range step.Attempts {
			step.Attempts[j].NodeID, step.Attempts[j].PodName = "", ""
		}
	}
}

func ownNativeStep(r *stateport.Run, step stepmodel.RunStep) bool {
	return step.Diagnostic != nil && r.Origin.NativeUID() != "" && step.Diagnostic.WorkflowUID == r.Origin.NativeUID()
}

func bindNativeFailure(r *stateport.Run) {
	if r.Failure == nil {
		return
	}
	for _, entry := range r.Admission.Plan.Entries {
		if entry.Path == r.Failure.Step {
			r.Failure.Name = entry.Name
			return
		}
	}
	for _, step := range r.Steps {
		if step.Path == r.Failure.Step && ownNativeStep(r, step) {
			r.Failure.Name = step.Name
			return
		}
	}
	r.Failure.Step, r.Failure.Name = "", ""
}
