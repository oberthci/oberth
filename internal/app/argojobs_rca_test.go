package app

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/runprogress"
)

func TestArgoResultPreservesStartedEarlierAttemptForAutomaticRetry(t *testing.T) {
	now := time.Now()
	completion := argojob.Completion{ReplayAttempts: []argojob.StepResult{{Burn: "ci", Step: "unit", Status: runprogress.StepPassed, StartedAt: &now}, {Burn: "ci", Step: "unit", Status: runprogress.StepQueued}}}
	if result := (&ArgoJobs{}).result(completion); !result.ObservedStepStart {
		t.Fatal("latest queued attempt concealed an earlier execution")
	}
}
