package runlog

// Focus is the retained selector and whether its active step can be followed
// through the caller's native log transport. It opens no live stream itself.
type Focus struct {
	Selector Selector
	Follow   bool
}

// SelectStep chooses the recorded failed step, then the running step, when the
// request omits a step/attempt. Candidates are paths from the authoritative run
// projection; nil means absent, while an empty path identifies the logical root.
// Explicit selectors win, and a container filter survives default selection.
// Without either candidate the whole-run selector remains available.
func SelectStep(requested Selector, failedStep, runningStep *string) Focus {
	if requested.NodeID != "" {
		return Focus{Selector: requested}
	}
	if requested.StepSet || requested.Step != "" {
		return Focus{Selector: requested, Follow: runningStep != nil && requested.Step == *runningStep}
	}
	if failedStep != nil {
		requested.Step, requested.StepSet = *failedStep, true
		return Focus{Selector: requested, Follow: runningStep != nil && *failedStep == *runningStep}
	}
	if runningStep != nil {
		requested.Step, requested.StepSet = *runningStep, true
		return Focus{Selector: requested, Follow: true}
	}
	return Focus{Selector: requested}
}
