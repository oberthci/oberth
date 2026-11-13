package app

import (
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
)

func operatorShutdownResult(result service.JobResult, completion argojob.Completion) service.JobResult {
	if completion.OperatorStopped {
		result.Status, result.Phase = model.RunInterrupted, model.PhaseCancelled
		result.FailedStage, result.FailedStep = "", ""
		result.Error = "Workflow stopped: " + completion.Reason
	}
	return result
}
