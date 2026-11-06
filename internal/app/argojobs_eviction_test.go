package app

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
)

func TestArgoResultPreservesEvictionProofAndFailedStep(t *testing.T) {
	at := time.Now().UTC()
	step := argojob.StepResult{Stage: "ci", Step: "install-helm", Status: runprogress.StepFailed, StartedAt: &at, FinishedAt: &at, InfrastructureReason: "Evicted: kubelet storage pressure"}
	c := argojob.Completion{Name: "workflow", Terminal: true, ObservationComplete: true, ObservedNodes: 2, Infrastructure: true, Reason: step.InfrastructureReason, FailedStage: step.Stage, FailedStep: step.Step, Steps: []argojob.StepResult{step}, ReplayAttempts: []argojob.StepResult{step}}
	result := (&ArgoJobs{}).result(c)
	if result.Phase != "infrastructure" || result.FailedStep != "" || !result.TerminalObservation.EvictionOnly() || len(result.Steps) != 1 || result.Steps[0].Status != model.StepFailed {
		t.Fatalf("eviction evidence lost: %+v", result)
	}
	c.ReplayAttempts[0].InfrastructureReason = "main: Error (exit code 1): Evicted"
	if (&ArgoJobs{}).result(c).TerminalObservation.EvictionOnly() {
		t.Fatal("application failure became an eviction")
	}
	c.ReplayAttempts[0] = step
	c.ReplayAttempts[0].AttemptAmbiguous = true
	if (&ArgoJobs{}).result(c).TerminalObservation.EvictionOnly() {
		t.Fatal("ambiguous attempt became an eviction")
	}
}
