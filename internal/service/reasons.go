package service

import (
	"errors"
	"fmt"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stepmodel"
)

const (
	reasonPromotionUnavailable     = "promotion record unavailable"
	reasonPromotionEvidenceInvalid = "promotion evidence invalid"
	warningAuthorityIncomplete     = "Authority application incomplete."
	warningAuthorityRetry          = "Retry the grant or revocation."
	remedyStaleLock                = "A run that ended still blocks this lock; restart the workflow controller to release it."
	warningSchedulesPaused         = "Saved; its schedules stay paused."
	warningSchedulesRetry          = "Repeat this command when no error shows in oberth schedule list --repo %s."
	reasonIssueUpdated             = "issue updated"
	reasonAuthorityIncomplete      = "authority application incomplete"
	reasonPubPending               = "publication pending"
	reasonPubAbandoned             = "publication abandoned"
	reasonPubMoved                 = "ref moved"
	reasonPubFailed                = "publication failed"
	reasonPubRetry                 = "publication retry deferred"
	reasonPubReceive               = "waiting for a pending receive"
	reasonPubDelivered             = "publication delivered"
	reasonReleased                 = "release published"
	reasonPromotePassed            = "promotion passed"
	reasonPromoteStart             = "promotion could not start"
	reasonPromoteMoved             = "target moved; retesting"
	reasonPromoteAgain             = "default branch moved; promote again"
	reasonRetestNotStarted         = "default branch moved; retest not started"
	reasonAssisted                 = "run is assisted"

	reasonHoldAbandon = "hold release requested"

	reasonQueued      = "waiting for execution"
	reasonRunning     = "run is running"
	reasonPassed      = "run passed"
	reasonFailed      = "run failed"
	reasonCancelled   = "run cancelled"
	reasonSkipped     = "step skipped"
	reasonPending     = "step not reached"
	reasonUnknown     = "execution evidence unavailable"
	reasonParked      = "branch parked"
	reasonDeleted     = "deletion recorded"
	reasonPurged      = "logs purged"
	reasonRelease     = "release steps may run again"
	reasonEscalate    = "repeat cancel to skip exit handlers"
	reasonPartial     = "publication may be partial"
	reasonStepQueued  = "step queued"
	reasonStepRunning = "step is running"
	reasonStepPassed  = "step passed"
	reasonStepFailed  = "step failed"
	reasonHeld        = "run is held"
)

type problem int

const (
	missing problem = iota
	invalid
	forbidden
	installation
	unavailable
	ambiguous
	moved
	active
	forceRequired
	promoteRequired
	archived
	publicationReceipt
	publicationEvidence
	publicationInterrupted
	promotionPolicyInvalid
	promotionForbidden
	promotionEvidence
	promotionConflict
	syncProtected
	holdInactive
	issueTooLarge
	backupRunning
	invitationRefused
	ciIssueManaged
	issueMissing
	runMissing
	invitationAddressChanged
	invitationAddressMismatch
	issueManyTooLarge
	repositoryRevocationPending
	unknownUpstreams
	issueUpstreamRequired
	promotionCurrent
	grantArgumentMissing
	grantArgumentUnused
	promotionHeadInvalid
)

type diagnostic struct {
	code           publicerr.Code
	reason, remedy string
}

func diagnostics() []diagnostic {
	return []diagnostic{
		{publicerr.NotFound, "The requested object was not found.", "Select a visible repository or run."},
		{publicerr.InvalidInput, "A named field is invalid.", "Correct the named field and retry."},
		{publicerr.Forbidden, "The caller is not authorized for this action.", "Use an authorized key for this repository."},
		{publicerr.Forbidden, "This action requires an installation administrator.", "Use an administrator key with no repository limit."},
		{publicerr.Unavailable, "A required service is unavailable.", "Retry when the service is available."},
		{publicerr.Ambiguous, "The selector matches several objects.", "Use the full canonical repository or run ID."},
		{publicerr.Conflict, "The selected ref has moved.", "Push again or rerun the current head."},
		{publicerr.InvalidTransition, "The run does not allow this action.", "Wait for completion or select a failed or cancelled run."},
		{publicerr.InvalidInput, "This tag or release cancellation requires force.", "Set force to confirm that publication may be partial."},
		{publicerr.Forbidden, "Agent pushes cannot update the default branch.", "Use promote to move the exact tested commit to the default branch."},
		{publicerr.InvalidTransition, "The repository is archived.", "Ask an administrator to unarchive the repository."},
		{publicerr.Unavailable, "The upstream receipt does not confirm this publication.", "Read publication_list and retry the exact intent."},
		{publicerr.Forbidden, "This run cannot provide publication evidence.", "Use an unassisted passing run for the exact commit."},
		{publicerr.Unavailable, "Publication was interrupted.", "Wait for recovery or run publish_retry on this publication."},
		{publicerr.InvalidInput, "The fresh default-branch promotion policy is invalid.", "Fix .oberth/promotion.yaml on a branch, pass CI, and ask an administrator to promote that commit."},
		{publicerr.Forbidden, "This repository does not allow uplink promotion.", "Ask an administrator to promote it, or to enable uplinks_may_promote in the default-branch policy."},
		{publicerr.Forbidden, "The exact source lacks passing unassisted branch evidence.", "Push and pass branch CI for this exact commit, then promote again."},
		{publicerr.Conflict, "The source conflicts with the current default branch.", "Resolve the conflicts on a branch, pass CI and promote its exact commit."},
		{publicerr.Forbidden, "Sync cannot publish this protected branch or run.", "Use promote for the default branch and release admission for tags."},
		{publicerr.InvalidTransition, "The selected hold is no longer available.", "List current holds and select the current native attempt."},
		{publicerr.InvalidInput, "The complete issue exceeds the response budget.", "Read issue_list metadata and request fewer complete issues."},
		{publicerr.Conflict, "backup already running", "Wait for the current backup stream to close."},
		{publicerr.Forbidden, "The invitation could not be redeemed.", "Ask the administrator for a new invitation."},
		{publicerr.InvalidTransition, "CI issues close when a later run passes.", "Pass a later scheduled or default-branch run, or ask an administrator to close it."},
		{publicerr.NotFound, "The requested issue was not found.", "Select a visible issue."},
		{publicerr.NotFound, "Run not found.", "List runs with oberth run list and select a visible run."},
		{publicerr.Forbidden, "The invitation address has changed.", "Ask the administrator to run oberth invite for a new invitation."},
		{publicerr.InvalidInput, "Invitation address must match the public address.", "Correct server_url in the selected profile's installation.json, then run oberth invite <upstream>."},
		{publicerr.InvalidInput, "The complete issue exceeds the combined response budget.", "Read issue %d by itself with issue_get or issue_get_many."},
		{publicerr.Unavailable, "Repository removal is waiting for grant revocation.", "Retry repository removal to finish applying the revoked grants."},
		{publicerr.InvalidInput, "Some upstream names are not registered.", "Register with repo add --upstream <name>; for issue import, map with --upstream <local>."},
		{publicerr.InvalidInput, "Choose one registered upstream.", "Use --upstream <name>; if none are listed, register a repository with repo add first."},
		{publicerr.InvalidTransition, "The default branch is already at this commit.", "Nothing to promote; push new work to a branch and promote its tested commit."},
		{publicerr.InvalidInput, "This grant kind needs the %s argument.", "Pass every argument the grant kind takes; oberth help access allow lists them."},
		{publicerr.InvalidInput, "This grant kind takes no %s argument.", "Drop that argument, or choose the grant kind it belongs to with kind."},
		{publicerr.InvalidInput, "The default branch head is not a readable branch.", "Check the repository's default branch on the forge, then retry the promotion."},
	}
}
func issueReadAlone(id int64) *publicerr.Error {
	d := diagnostics()[issueManyTooLarge]
	return publicerr.New(d.code, d.reason, fmt.Sprintf(d.remedy, id), nil)
}
func refuse(p problem) *publicerr.Error {
	d := diagnostics()[p]
	return publicerr.New(d.code, d.reason, d.remedy, nil)
}

// archivedRepository carries the command that reactivates the caller's own repository.
func archivedRepository(repo string) error {
	return refuse(archived).WithCommand(publicerr.UnarchiveCommand(repo))
}

// uplinkPromotion carries the administrator's promotion of the exact requested
// commit, which the default-branch policy does not restrict.
func uplinkPromotion(repo, sha string) error {
	p := refuse(promotionForbidden)
	if validID(sha) {
		p = p.WithCommand(publicerr.PromoteCommand(repo, sha))
	}
	return p
}

// invalidPromotionPolicy carries the administrator's promotion of the exact
// requested commit, which may be the one that corrects the policy file.
func invalidPromotionPolicy(repo, sha string) error {
	p := refuse(promotionPolicyInvalid)
	if validID(sha) {
		p = p.WithCommand(publicerr.PromoteCommand(repo, sha))
	}
	return p
}

// grantArgument names the access argument a grant kind needs or refuses.
func grantArgument(name string, missing bool) error {
	d := diagnostics()[grantArgumentUnused]
	if missing {
		d = diagnostics()[grantArgumentMissing]
	}
	return publicerr.New(d.code, fmt.Sprintf(d.reason, name), d.remedy, nil)
}
func bad(field string) error {
	d := diagnostics()[invalid]
	e := publicerr.New(d.code, "Invalid field: "+field+".", d.remedy, nil)
	return e
}
func terminal(status stepmodel.RunStatus) bool {
	return status == stepmodel.RunPassed || status == stepmodel.RunFailed || status == stepmodel.RunCancelled
}

func isMissing(err error) bool {
	var p *publicerr.Error
	return errors.As(err, &p) && p.Code() == publicerr.NotFound
}

func stepReason(status stepmodel.StepStatus) string {
	switch status {
	case stepmodel.StepPending:
		return reasonPending
	case stepmodel.StepQueued:
		return reasonStepQueued
	case stepmodel.StepRunning:
		return reasonStepRunning
	case stepmodel.StepPassed:
		return reasonStepPassed
	case stepmodel.StepFailed:
		return reasonStepFailed
	case stepmodel.StepSkipped:
		return reasonSkipped
	}
	return reasonUnknown
}

// summary fills only absent catalogue reasons; native detail remains separate.
func summary(r stepmodel.RunSummary) stepmodel.RunSummary {
	v := r.Clone()
	reason := runReason(v.Status)
	if v.DisplayReason == "" {
		v.DisplayReason = reason
	}
	return v
}

func runReason(status stepmodel.RunStatus) string {
	switch status {
	case stepmodel.RunQueued:
		return reasonQueued
	case stepmodel.RunRunning:
		return reasonRunning
	case stepmodel.RunPassed:
		return reasonPassed
	case stepmodel.RunFailed:
		return reasonFailed
	case stepmodel.RunCancelled:
		return reasonCancelled
	}
	return reasonUnknown
}
