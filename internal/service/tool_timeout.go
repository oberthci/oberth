package service

import "fmt"

// timeoutArguments is a blocking tool's own bound in seconds. timeout_s is
// the documented spelling of every long poll (wait, promote_status,
// run_wait, events_wait) and of run_hold_exec; timeout and timeout_seconds
// are accepted as aliases, so an agent that learned one tool is not refused
// by the next. Two spellings with different values are refused.
type timeoutArguments struct {
	TimeoutS       *int `json:"timeout_s"`
	Timeout        *int `json:"timeout"`
	TimeoutSeconds *int `json:"timeout_seconds"`
}

// seconds returns the one value given, or zero when none is.
func (arguments timeoutArguments) seconds() (int, error) {
	value, name := 0, ""
	for _, spelling := range []struct {
		name  string
		value *int
	}{{"timeout_s", arguments.TimeoutS}, {"timeout", arguments.Timeout}, {"timeout_seconds", arguments.TimeoutSeconds}} {
		if spelling.value == nil {
			continue
		}
		if name != "" && *spelling.value != value {
			return 0, fmt.Errorf("%w: %s and %s name different timeouts; pass timeout_s alone", ErrInvalidInput, name, spelling.name)
		}
		value, name = *spelling.value, spelling.name
	}
	return value, nil
}
