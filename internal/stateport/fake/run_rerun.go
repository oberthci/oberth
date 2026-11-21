package fake

import (
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func (m *runMemory) rerun(a stateport.Acceptance) error {
	if a.RerunOf == "" {
		return nil
	}
	r, ok := m.runs[a.RerunOf]
	if !ok {
		return runProblem(publicerr.NotFound)
	}
	if r.Repo != a.Request.Repo || r.Ref != a.Request.Ref {
		return runProblem(publicerr.Conflict)
	}
	return nil
}

func finishRemainders(r *stateport.Run) {
	if r.Steps != nil {
		return
	}
	for _, entry := range r.Admission.Plan.Entries {
		if entry.Dynamic {
			r.Pending--
			r.Unknown++
		}
	}
}
