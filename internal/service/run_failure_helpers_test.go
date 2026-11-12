package service

import (
	"context"
)

type rcaCreateFailure struct {
	recoveryJobs
	createErr   error
	createCalls int
}

func (j *rcaCreateFailure) CreateWorkflow(context.Context, WorkflowRequest) error {
	j.createCalls++
	return j.createErr
}
