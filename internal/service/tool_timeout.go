package service

// timeoutArguments is a blocking tool's own bound in seconds: timeout_s, the
// one spelling of every long poll (wait, promote_status, run_wait,
// events_wait) and of run_hold_exec.
type timeoutArguments struct {
	TimeoutS *int `json:"timeout_s"`
}

// seconds returns timeout_s, or zero when it is omitted.
func (arguments timeoutArguments) seconds() int {
	if arguments.TimeoutS == nil {
		return 0
	}
	return *arguments.TimeoutS
}
