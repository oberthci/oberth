package store

import (
	"database/sql"
	"errors"
	"syscall"

	"github.com/oberthci/oberth/internal/publicerr"
	"modernc.org/sqlite"
)

type refusal int

const (
	invalid refusal = iota
	missing
	conflict
	unavailable
	schema
	foreignProtocol
	newerSchema
	forbidden
	corrupt
	ambiguous
	transition
	space
	tooLarge
	locked
	diskFull
	damagedArchive
	stageBusy
	exists
	staleRevision
	terminalRun
	pendingPublication
	bindingChanged
	assistNotRequested
	purgeProtected
	purgeRunning
	purgeReceiptChanged
	incompleteEvidence
	assistedEvidence
	attemptChanged
	publicationSettled
	lockHeld
	issueChanged
	nodeBinding
	nodeRegression
	nodeRetention
	eventDedupe
	repositoryBound
	repositoryBusy
	upstreamBound
	upstreamChanged
	foreignOrganization
	claimBound
	invitationHeld
	invitationExpired
	lastAdmin
	receiveChanged
	noAdmission
	attemptInFlight
	retryDeferred
	workflowBound
	unboundReason
	invalidTrigger
	invalidGlob
	invalidCommit
	ambiguousCommit
	scheduleAdmissionExpired
)

func refuse(kind refusal, cause error) error {
	code, reason, remedy := message(kind)
	return publicerr.New(code, reason, remedy, cause)
}

type diagnostic struct {
	code           publicerr.Code
	reason, remedy string
}

func message(kind refusal) (publicerr.Code, string, string) {
	d, ok := diagnostics()[kind]
	if !ok {
		panic("unknown state refusal")
	}
	return d.code, d.reason, d.remedy
}

func databaseError(err error) error {
	if err == nil {
		return nil
	}
	var public *publicerr.Error
	if errors.As(err, &public) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		return refuse(missing, err)
	}
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return refuse(diskFull, err)
	}
	var database *sqlite.Error
	if errors.As(err, &database) {
		switch database.Code() & 255 {
		case 5:
			return refuse(locked, err)
		case 13:
			return refuse(diskFull, err)
		case 19:
			return refuse(invalid, err)
		case 11, 26:
			return refuse(corrupt, err)
		}
	}
	return refuse(unavailable, err)
}

func measuredRefusal(kind refusal, detail string) error {
	code, reason, remedy := message(kind)
	err := publicerr.New(code, reason, remedy, nil)
	err.Detail = detail
	return err
}
