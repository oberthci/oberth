package app

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/runprogress"
)

func TestReplayObservationMappingRetainsAllAttempts(t *testing.T) {
	now := time.Now()
	jobs := &ArgoJobs{}
	completion := argojob.Completion{Name: "actual-workflow", ObservationComplete: true, ObservedNodes: 3, ReplayAttempts: []argojob.StepResult{
		{Burn: "publish", Step: "push", Status: runprogress.StepFailed, StartedAt: &now},
		{Burn: "publish", Step: "push", Status: runprogress.StepSkipped},
	}}
	result := jobs.result(completion)
	if result.TerminalObservation == nil || result.TerminalObservation.JobName != completion.Name || len(result.TerminalObservation.Steps) != 2 || !result.TerminalObservation.Steps[0].Started {
		t.Fatalf("earlier start lost in engine mapping: %+v", result.TerminalObservation)
	}
	completion.ObservationComplete = false
	if jobs.result(completion).TerminalObservation != nil {
		t.Fatal("incomplete observation acquired a proof")
	}
}
