package gitcache

import "github.com/oberthci/oberth/internal/publicerr"

// RefusalError keeps fixed guidance separate from internal field attribution.
type RefusalError struct {
	reason, remedy string
	command        string
	Field          string
	Detail         string
}

func (e *RefusalError) Reason() string { return e.reason }
func (e *RefusalError) Remedy() string { return e.remedy }
func (e *RefusalError) Error() string {
	text := e.reason + ". " + e.remedy
	if e.Detail != "" {
		text += " " + e.Detail
	}
	if e.command != "" {
		text += "\n" + publicerr.AdministratorLine(e.command)
	}
	return text
}
func (e *RefusalError) Unwrap() error {
	return publicerr.New(publicerr.InvalidInput, e.reason, e.remedy, nil).WithCommand(e.command)
}

// pushBusy refuses a push while the repository's earlier delivery waits for
// an administrator, and names the retry of that delivery.
func pushBusy(repository string) error {
	return &RefusalError{reason: reasonPushBusy, remedy: remedyPushBusy, command: publicerr.ReceiveRetryCommand(repository), Field: "previous receive"}
}

// Conditions that callers can remedy have their own literal guidance.
func refusalText(field string) (string, string) {
	switch field {
	case "receive update count":
		return reasonUpdateCount, remedyUpdateCount
	case "receive transaction":
		return reasonTransactionPhase, remedyTransactionPhase
	case "promotion result":
		return reasonPromotionExpired, remedyPromotionExpired
	case "immutable tag":
		return reasonTag, remedyTag
	case "protected branch":
		return reasonProtected, remedyProtected
	case "deletion admission":
		return reasonDeletion, remedyDeletion
	case "default fast-forward", "publication fast-forward":
		return reasonForward, remedyForward
	case "newer local head":
		return reasonMoved, remedyMoved
	case "pending receive":
		return reasonPending, remedyPending
	}
	return reasonInvalid, remedyInvalid
}

type reportedError struct{ error }

func (*reportedError) Reported() bool  { return true }
func (e *reportedError) Unwrap() error { return e.error }

type deliveryPendingError struct{ error }

func (*deliveryPendingError) Reason() string  { return reasonDeliveryPending }
func (*deliveryPendingError) Remedy() string  { return remedyDeliveryPending }
func (e *deliveryPendingError) Error() string { return e.Reason() + ". " + e.Remedy() }
func (e *deliveryPendingError) Unwrap() error { return e.error }

func (e *deliveryPendingError) GitFeedback() string { return e.Error() }

type receiveRollbackError struct{ error }

func (*receiveRollbackError) Reason() string { return reasonRollback }
func (*receiveRollbackError) Remedy() string { return remedyRollback }
func (e *receiveRollbackError) Unwrap() error {
	return publicerr.New(publicerr.Unavailable, e.Reason(), e.Remedy(), e.error)
}
func (e *receiveRollbackError) GitFeedback() string { return e.Reason() + ". " + e.Remedy() }

type receiveCleanupPendingError struct{ error }

func (*receiveCleanupPendingError) Reason() string { return reasonDeliveryPending }
func (*receiveCleanupPendingError) Remedy() string { return remedyReceiveCleanup }
func (e *receiveCleanupPendingError) Unwrap() error {
	return publicerr.New(publicerr.Unavailable, e.Reason(), e.Remedy(), e.error)
}

type receiveRepairError struct{ error }

func (*receiveRepairError) Reason() string { return reasonReceiveRepair }
func (*receiveRepairError) Remedy() string { return remedyReceiveRepair }
func (e *receiveRepairError) Unwrap() error {
	return publicerr.New(publicerr.Unavailable, e.Reason(), e.Remedy(), e.error)
}
func (e *receiveRepairError) GitFeedback() string { return e.Reason() + ". " + e.Remedy() }
