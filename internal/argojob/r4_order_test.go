package argojob

import (
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"testing"
)

func TestAttemptMergeUsesRawNumberWhenNativeOrderAndTimesAreAbsent(t *testing.T) {
	t.Parallel()
	old := stepmodel.RunStep{Attempts: []stepmodel.StepAttempt{{NodeID: "later", Status: stepmodel.StepFailed}}}
	current := stepmodel.RunStep{Attempts: []stepmodel.StepAttempt{{NodeID: "earlier", Status: stepmodel.StepFailed}}}
	raw := map[string]stateport.Node{"earlier": {Attempt: 2, Status: stepmodel.StepFailed}, "later": {Attempt: 9, Status: stepmodel.StepFailed}}
	got := mergeAttempts(current, old, raw)
	if len(got) != 2 || got[0].NodeID != "earlier" || got[1].NodeID != "later" {
		t.Fatal("arrival order replaced native attempt numbers", got)
	}
}
