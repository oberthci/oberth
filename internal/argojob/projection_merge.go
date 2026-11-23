package argojob

import "github.com/oberthci/oberth/internal/stepmodel"

func primaryFailure(rows []projected) *stepmodel.FailureRecord {
	var first *projected
	allInfrastructure := true
	for i := range rows {
		r := &rows[i]
		if !failedProjection(*r) {
			continue
		}
		if r.class != stepmodel.FailureInfrastructure {
			allInfrastructure = false
		}
		if first == nil || finishedEarlier(r, *first) {
			first = r
		}
	}
	if first == nil {
		return nil
	}
	class := first.class
	if class == "" || class == stepmodel.FailureInfrastructure && !allInfrastructure {
		class = stepmodel.FailureUnknown
	}
	retry := stepmodel.RetryNone
	if allInfrastructure {
		class, retry = stepmodel.FailureInfrastructure, stepmodel.RetryRerun
	}
	id := latestNode(first.step.Attempts)
	hint := stepmodel.RetryHint{Action: retry}
	if class == stepmodel.FailureAuthor {
		hint.Detail = "Oberth cannot tell whether rerunning unchanged is safe. Inspect the failure and logs; workflow YAML governs retries."
	}
	return &stepmodel.FailureRecord{Class: class, Step: first.step.Path, Name: first.step.Name, NodeID: id, Retry: hint}
}
func failedProjection(r projected) bool {
	return r.step.Status == stepmodel.StepFailed || r.step.Status == stepmodel.StepUnknown && r.step.AttemptCount > 0
}
func latestNode(attempts []stepmodel.StepAttempt) string {
	if len(attempts) == 0 {
		return ""
	}
	return attempts[len(attempts)-1].NodeID
}

func terminalStep(s stepmodel.StepStatus) bool {
	return s == stepmodel.StepFailed || s == stepmodel.StepPassed || s == stepmodel.StepSkipped
}

func finishedEarlier(a *projected, b projected) bool {
	return a.step.FinishedAt != nil && (b.step.FinishedAt == nil || a.step.FinishedAt.Before(*b.step.FinishedAt))
}
