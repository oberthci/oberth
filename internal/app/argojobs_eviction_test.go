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
	result := (&ArgoJobs{}).result(fixtureCompletionNodes(c))
	if result.Phase != "infrastructure" || result.FailedStep != "" || !result.TerminalObservation.EvictionOnly() || len(result.Steps) != 1 || result.Steps[0].Status != model.StepFailed {
		t.Fatalf("eviction evidence lost: %+v", result)
	}
	c.ReplayAttempts[0].InfrastructureReason = "main: Error (exit code 1): Evicted"
	if (&ArgoJobs{}).result(fixtureCompletionNodes(c)).TerminalObservation.EvictionOnly() {
		t.Fatal("application failure became an eviction")
	}
	c.ReplayAttempts[0] = step
	c.ReplayAttempts[0].AttemptAmbiguous = true
	if (&ArgoJobs{}).result(fixtureCompletionNodes(c)).TerminalObservation.EvictionOnly() {
		t.Fatal("ambiguous attempt became an eviction")
	}
}

func TestArgoResultPreservesRebootAndSkippedPublication(t *testing.T) {
	at := time.Now().UTC()
	failed := argojob.StepResult{Stage: "ci", Step: "test", Status: runprogress.StepFailed, ExitCode: 255, StartedAt: &at, FinishedAt: &at, InfrastructureReason: model.NodeRebootReasonPrefix + "node worker restarted"}
	skipped := argojob.StepResult{Stage: "publish", Step: "push", Status: runprogress.StepSkipped, StartedAt: &at, FinishedAt: &at}
	c := argojob.Completion{Name: "workflow", Terminal: true, ObservationComplete: true, ObservedNodes: 3, Infrastructure: true, Reason: failed.InfrastructureReason, Steps: []argojob.StepResult{failed, skipped}, ReplayAttempts: []argojob.StepResult{failed, skipped}}
	result := (&ArgoJobs{}).result(fixtureCompletionNodes(c))
	if result.Phase != "infrastructure" || !result.TerminalObservation.EvictionOnly() || !result.TerminalObservation.Steps[0].NodeReboot || result.TerminalObservation.Steps[1].Started {
		t.Fatalf("evidence: %+v", result.TerminalObservation)
	}
	c.Steps, c.ReplayAttempts = []argojob.StepResult{skipped}, []argojob.StepResult{skipped}
	if result = (&ArgoJobs{}).result(fixtureCompletionNodes(c)); result.ObservedStepStart {
		t.Fatal("skipped node timestamp counted as execution")
	}
}
