package sourceplugin

const (
	remedyTooManyFiles = "Reduce the number of pinned source files and push again."
	reasonTagMoved     = "tag moved since admission"

	reasonConfig   = "source plugin configuration unavailable"
	reasonInput    = "invalid source artifact"
	reasonIdentity = "executor identity unavailable"
	reasonTransfer = "source transfer unavailable"
	reasonDenied   = "source access is forbidden"
	reasonReceipt  = "source receipt is invalid"
	reasonFile     = "source artifact file unavailable"
	reasonOutput   = "source output is unsupported"
	reasonSocket   = "absolute Unix socket path required"
	reasonDisabled = "source delivery is disabled"
	reasonWaiting  = "waiting for the Oberth server"
	remedyConfig   = "Check the source endpoint, CA, server name and Pod token file."
	remedyInput    = "Use an Oberth input descriptor and the admitted run locator."
	remedyIdentity = "Check the projected Pod token file at the reported path."
	remedyRetry    = "Retry the step; if source is gone, push again."
	remedyDenied   = "Use a live Pod of the recorded Workflow, or push again for a new run."
	remedyFile     = "Provide writable helper scratch and an absolute artifact path."
	remedyOutput   = "Declare source only as an input artifact."
	remedySocket   = "Pass one absolute Unix socket path."
)

type failureError struct{ reason string }

func (e *failureError) Reason() string { return e.reason }
func (e *failureError) Remedy() string {
	switch e.reason {
	case reasonConfig, reasonDisabled:
		return remedyConfig
	case reasonInput:
		return remedyInput
	case reasonIdentity:
		return remedyIdentity
	case reasonDenied:
		return remedyDenied
	case reasonFile:
		return remedyFile
	case reasonOutput:
		return remedyOutput
	case reasonSocket:
		return remedySocket
	}
	return remedyRetry
}
func (e *failureError) Error() string { return e.Reason() + ". " + e.Remedy() }
func failure(reason string) error     { return &failureError{reason: reason} }
