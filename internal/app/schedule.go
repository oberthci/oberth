package app

import (
	"context"
	"strings"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
)

// PrepareSchedule admits a reusable parent. Deferred findings cannot authorize
// a Workflow which the native controller may create without another admission.
func (s *Submitter) PrepareSchedule(ctx context.Context, r Request) (*admissioncontract.PreparedAdmission, error) {
	if strings.Contains(r.Ref.Name, "{{") {
		err := refusal("schedule_ref", nil)
		err.Ref = r.Ref.Name
		return nil, err
	}
	r.RunID, r.Trigger = "", model.TriggerSchedule
	run, err := s.admit(ctx, r, s.d.Source, admitOptions{mode: admissioncontract.ModeInspect})
	if err != nil {
		return nil, err
	}
	view := run.Prepared.View()
	var failures []admissioncontract.Finding
	for _, f := range view.Findings {
		if !f.Advisory {
			failures = append(failures, f)
		}
	}
	if len(failures) > 0 {
		return nil, &Refusal{Code: "prepare", Problems: failures}
	}
	if err := scheduleExpressions(view.Workflow.Spec); err != nil {
		return nil, err
	}
	return run.Prepared, nil
}
