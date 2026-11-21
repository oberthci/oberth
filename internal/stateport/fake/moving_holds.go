package fake

import (
	"context"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func (f *Runs) Assist(ctx context.Context, a stateport.Action, q stateport.HoldAction) error {
	if !q.Valid() || a.Actor == "" {
		return runProblem(publicerr.InvalidInput)
	}
	return f.movingChange(ctx, "Assist", a.Events, func(m *runMemory) error {
		r, ok := m.runs[q.RunID]
		if !ok {
			return runProblem(publicerr.NotFound)
		}
		if r.Revision != q.Revision {
			return runProblem(publicerr.Conflict)
		}
		if !r.Origin.AssistRequested {
			return runProblem(publicerr.Forbidden)
		}
		if !stateport.NativePlanPath(r.Admission.Plan, q.Step) {
			return runProblem(publicerr.InvalidInput)
		}
		if q.Kind == "exec" {
			r.Origin.Assisted = true
		}
		r.Revision++
		m.runs[r.ID] = r.Clone()
		m.appendAudit(model.AuditAction{Actor: a.Actor, Action: "run.assist", ResourceType: "run", ResourceID: r.ID, Details: jsonValue(q.Audit(a.Reason))})
		return nil
	})
}
