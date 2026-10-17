package runlog

import (
	"regexp"
	"strings"
)

// Argo's executor (argoexec emissary) runs as PID 1 of every step's main
// container and logs its own lifecycle into the step's stream at the
// controller's level: "waiting for signals", "sub-process exited"
// error=<nil>, "file signal handler exiting due to context cancellation".
// They are logfmt lines that begin time=… level=… and carry argo=true.
var (
	executorLineRegexp = regexp.MustCompile(`(?i)^time=\S+ level=(info|debug) (.*\s)?argo=true(\s|$)`)
	executorErrRegexp  = regexp.MustCompile(`(?i)(^|[\s,;])(err|errs|error|errors)=("[^"]*"|'[^']*'|\S*)`)
	// executorStepPrefix is the dashboard's LOG_RULES.stepPrefix: a
	// [burn/step] marker, so both sides strip exactly the same prefix.
	executorStepPrefix = regexp.MustCompile(`^\[[^\]]*/[^\]]*\] `)
)

// executorNilErrors are the error= values that mean "no error".
var executorNilErrors = map[string]bool{"": true, `""`: true, "''": true, "<nil>": true, "nil": true, "null": true, "none": true, "<none>": true, "-": true}

// IsExecutorNoise reports whether a log line (with or without its
// [burn/step] prefix) is Argo executor chatter a person reading the step
// does not need: an INFO or DEBUG executor line whose error fields are all
// empty. WARN and ERROR lines and a non-nil error — "sub-process exited"
// error="exit status 1" is the only exit record in the log — are never
// noise. Presentation only: stored logs and the API keep every line.
func IsExecutorNoise(line string) bool {
	body := strings.TrimSpace(executorStepPrefix.ReplaceAllString(line, ""))
	if !executorLineRegexp.MatchString(body) {
		return false
	}
	for _, match := range executorErrRegexp.FindAllStringSubmatch(body, -1) {
		if !executorNilErrors[strings.ToLower(match[3])] {
			return false
		}
	}
	return true
}
