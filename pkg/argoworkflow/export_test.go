package argoworkflow

import (
	"strings"

	"github.com/oberthci/oberth/pkg/pipeline"
)

// FileTrigger is the inverse of TriggerFile.
func FileTrigger(file string) (pipeline.Trigger, bool) {
	switch strings.TrimSpace(file) {
	case BuildFile:
		return pipeline.TriggerCI, true
	case ReleaseFile:
		return pipeline.TriggerRelease, true
	default:
		return "", false
	}
}
