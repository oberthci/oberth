package store

import (
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func validateRejected(input stateport.Acceptance) error {
	r := input.Request
	rejected := input.Rejected
	if rejected == nil || rejected.ID == "" || input.Child != nil || input.Prepared != nil || len(rejected.Findings) == 0 {
		return refuse(invalid, nil)
	}
	if !rejected.Failure.Class.Valid() || !rejected.Failure.Retry.Action.Valid() || r.ObjectSHA == "" {
		return refuse(invalid, nil)
	}
	if err := validateFindings(rejected.Findings); err != nil {
		return err
	}
	return validateRequest(r)
}

func validateRequest(r model.CIRequest) error {
	if r.RequestedFile != "" && sourcecontract.ValidateFilePath(r.RequestedFile) != nil {
		return refuse(invalid, nil)
	}
	if !validRef(r.Ref) || !validReceiveID(r.ReceiveID) || r.Actor.Identity == "" || r.Actor.AuthMethod == "" {
		return refuse(invalid, nil)
	}
	return nil
}

func applyAdmission(run *stateport.Run, input stateport.Acceptance) {
	if input.Prepared != nil {
		run.HasAdmission = true
		run.Admission = input.Prepared.View()
		run.ID = run.Admission.RunID
		run.LogicalStepCounts = stepmodel.LogicalStepCounts{Total: len(run.Admission.Plan.Entries), Exact: run.Admission.Plan.Exact,
			Pending: len(run.Admission.Plan.Entries)}
		return
	}
	rejected := input.Rejected
	run.ID = rejected.ID
	run.Status = stepmodel.RunFailed
	run.Failure = &rejected.Failure
	run.Findings = append([]admissioncontract.Finding(nil), rejected.Findings...)
	run.FinishedAt = new(run.CreatedAt)
	run.DisplayReason = stateport.ReasonAdmission
}

func validateFindings(findings []admissioncontract.Finding) error {
	for _, finding := range findings {
		if finding.Code == "" || publicerr.CheckMessage(finding.Reason, 80) != nil || publicerr.CheckMessage(finding.Remedy, 120) != nil {
			return refuse(invalid, nil)
		}
	}
	return nil
}
