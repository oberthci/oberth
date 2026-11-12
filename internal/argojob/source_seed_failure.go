package argojob

// sourceSeedFailure keeps the complete operation error and a short, fixed
// reason that remains useful when the list truncates detailed object names.
type sourceSeedFailure struct {
	failure error
	reason  string
}

func (e *sourceSeedFailure) Error() string           { return e.failure.Error() }
func (e *sourceSeedFailure) Unwrap() error           { return e.failure }
func (e *sourceSeedFailure) RunFailureCause() string { return e.reason }
