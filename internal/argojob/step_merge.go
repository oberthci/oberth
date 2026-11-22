package argojob

import (
	"cmp"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"slices"
)

func mergeRows(rows []projected, previous []stepmodel.RunStep, plan admissioncontract.Plan, raw []stateport.Node) []projected {
	byPath := map[string]int{}
	nodes := map[string]stateport.Node{}
	for i, r := range rows {
		byPath[r.step.Path] = i
	}
	for _, n := range raw {
		nodes[n.NodeID] = n
	}
	for _, old := range previous {
		if i, ok := byPath[old.Path]; ok {
			rows[i] = mergeStep(rows[i], old, nodes)
		} else if !expandedPlaceholder(old, plan, rows) {
			rows = append(rows, projected{step: old.Clone(), class: stepmodel.FailureUnknown})
		}
	}
	return rows
}
func expandedPlaceholder(old stepmodel.RunStep, plan admissioncontract.Plan, rows []projected) bool {
	if old.AttemptCount != 0 {
		return false
	}
	return slices.ContainsFunc(plan.Entries, func(p admissioncontract.PlanEntry) bool {
		return p.Dynamic && old.Path == p.Path && slices.ContainsFunc(rows, func(r projected) bool {
			return r.step.Path != p.Path && underPath(r.step.Path, p.Path)
		})
	})
}
func mergeStep(row projected, old stepmodel.RunStep, raw map[string]stateport.Node) projected {
	current := row.step
	old = old.Clone()
	attempts := mergeAttempts(current, old, raw)
	latest := latestNode(attempts)
	if latest != latestNode(current.Attempts) || len(attempts) == 0 && terminalStep(old.Status) {
		row.step = old
		row.class = stepmodel.FailureUnknown
	} else if latest == latestNode(old.Attempts) {
		row = mergeDiagnosis(row, old)
	}
	if current.StartedAt == nil || old.StartedAt != nil && old.StartedAt.Before(*current.StartedAt) {
		row.step.StartedAt = old.StartedAt
	}
	row.step.Attempts, row.step.AttemptCount, row.step.Logs = attempts, len(attempts), mergeLogs(current.Logs, old.Logs)
	if len(attempts) > 0 {
		row.step.Status = attempts[len(attempts)-1].Status
		if n, ok := raw[latest]; ok && n.FinishedAt != nil {
			row.step.FinishedAt = n.FinishedAt
		}
	}
	return row
}
func mergeDiagnosis(row projected, old stepmodel.RunStep) projected {
	if terminalStep(old.Status) && (mergeStatus(old.Status, row.step.Status) != row.step.Status || row.step.DisplayReason == reasonFailed || row.step.DisplayReason == reasonStopped || row.step.DisplayReason == reasonUnknown) {
		row.step.DisplayReason, row.step.Detail, row.class = old.DisplayReason, old.Detail, stepmodel.FailureUnknown
	}
	// A step that passed since an unfinished observation keeps none of that
	// observation's waiting detail, such as PodInitializing.
	if row.step.Status != stepmodel.StepPassed || terminalStep(old.Status) {
		row.step.Detail = cmp.Or(row.step.Detail, old.Detail)
	}
	row.step.Name = cmp.Or(row.step.Name, old.Name)
	row.step.FinishedAt = cmp.Or(row.step.FinishedAt, old.FinishedAt)
	return row
}
