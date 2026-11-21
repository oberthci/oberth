package sourcedelivery

const (
	remedyTooLarge = "Reduce source or history size and push again."
	remedyGone     = "Push again to start a run with available source."
	remedyRequest  = "Retry the source request."

	reasonTooLarge        = "source exceeds the delivery limit"
	reasonGone            = "recorded source is no longer kept"
	reasonRequest         = "invalid source request"
	reasonReceipt         = "invalid source receipt"
	reasonUnauthenticated = "source identity is unverified"
	reasonForbidden       = "source access is forbidden"
	reasonUnavailable     = "source is temporarily unavailable"
	reasonUnconfigured    = "source delivery is not configured"
)
