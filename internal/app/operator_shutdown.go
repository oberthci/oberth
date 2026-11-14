package app

import (
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
)

func operatorShutdownResult(result service.JobResult, completion argojob.Completion) service.JobResult {
	if completion.OperatorStopped && !completion.Succeeded {
		result.Status = model.RunInterrupted
		result.Phase = model.PhaseCancelled
		result.FailedStage, result.FailedStep = "", ""
		result.Error = "Workflow stopped by its Workflow spec: " + completion.Reason
	}
	return result
}
