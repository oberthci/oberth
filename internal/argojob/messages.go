package argojob

import (
	"errors"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
)

const (
	helperImageReason = "Installation image is not digest-pinned"
	helperImageRemedy = "Ask the operator to install or upgrade Oberth with a digest-pinned release."
	execStartReason   = "Command could not start in the held step"
	execStartRemedy   = "Check the command exists in the step image and try again."
	execTimeoutReason = "Command timed out"
	execTimeoutRemedy = "Use a shorter command or a larger timeout_s, at most 900."
	absentReason      = "Workflow is absent"
	bindingReason     = "Workflow ownership differs"
	accountReason     = "Step account is missing"
	ciAccountReason   = "CI account for the step is missing"
	holdReason        = "Hold is no longer available"
	limitReason       = "Hold operation exceeds its limit"
	unavailableReason = "Native execution is unavailable"
	bindingRemedy     = "Inspect the persisted run and its named Workflow."
	accountRemedy     = "Install the admitted step account and retry the push."
	holdRemedy        = "Refresh the run holds and select its current Pod."
	limitRemedy       = "Use at most 15 minutes and 4 MiB for an inspection command."
	nativeRemedy      = "Read the run status before retrying the operation."
)

// ErrAbsent is returned only for a native NotFound, never an API outage.
func absent(cause error) error {
	return publicerr.New(publicerr.NotFound, absentReason, bindingRemedy, errors.Join(ErrAbsent{}, cause))
}

type ErrAbsent struct{}

func (ErrAbsent) Error() string { return absentReason }
func ownership(cause error) error {
	return publicerr.New(publicerr.Forbidden, bindingReason, bindingRemedy, errors.Join(&nativebinding.Refusal{}, cause))
}
func unavailable(cause error) error {
	return publicerr.New(publicerr.Unavailable, unavailableReason, nativeRemedy, cause)
}
func accountMissing(ci bool, cause error) error {
	reason := accountReason
	if ci {
		reason = ciAccountReason
	}
	e := publicerr.New(publicerr.Unavailable, reason, accountRemedy, cause)
	if cause != nil {
		e.Detail = cause.Error()
	}
	return e
}
func notHeld(cause error) error {
	return publicerr.New(publicerr.Conflict, holdReason, holdRemedy, cause)
}
func limited() error { return publicerr.New(publicerr.InvalidInput, limitReason, limitRemedy, nil) }

const (
	childNotRecorded      = "scheduled child not recorded"
	childAdmissionInvalid = "scheduled admission annotation is invalid"
	childAdmissionPending = "scheduled admission not found"
	childObserved         = "scheduled run observed"
	childRemedy           = "Check that the Workflow was created by its Oberth schedule."
)

func childError(code publicerr.Code, reason string, cause error) error {
	return publicerr.New(code, reason, childRemedy, cause)
}
