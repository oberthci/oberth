package argoworkflow

import (
	"strings"

	"github.com/oberthci/oberth/pkg/periapsis"
)

// FileTrigger is the inverse of TriggerFile.
func FileTrigger(file string) (periapsis.Trigger, bool) {
	switch strings.TrimSpace(file) {
	case BuildFile:
		return periapsis.TriggerCI, true
	case ReleaseFile:
		return periapsis.TriggerRelease, true
	default:
		return "", false
	}
}
