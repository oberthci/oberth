package argojob

import (
	"testing"

	"github.com/oberthci/oberth/internal/runprogress"
)

func TestInfrastructureClassificationKeepsFirstApplicationFailure(t *testing.T) {
	c := Completion{FailedStage: "test", FailedStep: "test", Steps: []StepResult{
		{Stage: "test", Step: "test", Status: runprogress.StepFailed},
		{Stage: "cleanup", Step: "cleanup", Status: runprogress.StepFailed},
		{Stage: "evicted", Step: "evicted", Status: runprogress.StepFailed, InfrastructureReason: "Evicted: node pressure"},
	}}
	c.classifyInfrastructure()
	if c.Infrastructure || c.FailedStep != "test" || c.FailedStage != "test" {
		t.Fatalf("classification replaced the first failure: %+v", c)
	}
}
