package main

import (
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/logcontract"
	"github.com/oberthci/oberth/internal/stepmodel"
	"time"
)

func (s *flowSession) logRunInfo(id string) (stepmodel.RunSummary, error) {
	var result stepmodel.RunSummary
	request := apicontract.RunGetArgs{ID: id}
	for {
		var page stepmodel.RunSummary
		if err := s.invoke("run_get", request, &page); err != nil {
			return result, err
		}
		steps := append(result.Steps, page.Steps...)
		page.Steps = steps
		result = page
		if page.NextStepOffset == nil {
			return result, nil
		}
		if *page.NextStepOffset <= request.StepOffset {
			return result, flowError("contract", nil)
		}
		request.StepOffset = *page.NextStepOffset
	}
}

func (s *flowSession) logPlannedSteps(request apicontract.RunLogsArgs) error {
	run, err := s.logRunInfo(request.ID)
	if err != nil {
		return err
	}
	shown := map[string]bool{}
	if request.Step != "" {
		if _, found := nextLogStep(run.Steps, request.Step, shown); !found {
			var logs apicontract.LogResult
			if err := s.invoke("run_logs", request, &logs); err != nil {
				return err
			}
			return flowError("contract", nil)
		}
	}
	for {
		step, found := nextLogStep(run.Steps, request.Step, shown)
		if !found && (logRunFinished(run) || request.Step != "") {
			return s.fact("run: %s\nresult: %s\nnext: oberth run get %s", run.ID, run.Status, run.ID)
		}
		if found {
			run, err = s.printLogStep(request, step, run, shown)
		} else {
			run, err = s.waitLogRun(request.ID)
		}
		if err != nil {
			return err
		}
	}
}

func (s *flowSession) printLogStep(request apicontract.RunLogsArgs, step stepmodel.RunStep, run stepmodel.RunSummary, shown map[string]bool) (stepmodel.RunSummary, error) {
	read, available := stepLogRequest(request, step)
	if !available && !logStepFinished(run, step.Path) {
		return s.waitLogRun(request.ID)
	}
	if err := s.fact("== %s ==", step.Name); err != nil {
		return run, err
	}
	if available {
		var err error
		run, err = s.followStepLog(read, step)
		if err != nil {
			return run, err
		}
	}
	shown[step.Path] = true
	return run, nil
}

func (s *flowSession) waitLogRun(id string) (stepmodel.RunSummary, error) {
	if err := s.app.config.WaitFor(s.ctx, time.Second); err != nil {
		return stepmodel.RunSummary{}, err
	}
	return s.logRunInfo(id)
}

// nextLogStep selects by plan path or by the name `run get` lists for a step.
func nextLogStep(steps []stepmodel.RunStep, selected string, shown map[string]bool) (stepmodel.RunStep, bool) {
	for _, step := range steps {
		if !shown[step.Path] && (selected == "" || step.Path == selected || step.Name == selected) {
			return step, true
		}
	}
	return stepmodel.RunStep{}, false
}

func stepLogRequest(request apicontract.RunLogsArgs, step stepmodel.RunStep) (apicontract.RunLogsArgs, bool) {
	request.Step = step.Path
	if step.Status == stepmodel.StepSkipped && len(step.Attempts) == 0 {
		return request, false
	}
	if step.Path == "" {
		if len(step.Attempts) == 0 {
			return request, false
		}
		request.NodeID = step.Attempts[len(step.Attempts)-1].NodeID
	}
	return request, true
}

func logStepFinished(run stepmodel.RunSummary, path string) bool {
	if logRunFinished(run) {
		return true
	}
	for _, step := range run.Steps {
		if step.Path == path {
			return step.Status == stepmodel.StepPassed || step.Status == stepmodel.StepFailed || step.Status == stepmodel.StepSkipped
		}
	}
	return false
}

func logRunFinished(run stepmodel.RunSummary) bool {
	return run.Status != stepmodel.RunQueued && run.Status != stepmodel.RunRunning
}

func logContainerKey(logs apicontract.LogResult) string {
	if len(logs.Containers) == 0 {
		return ""
	}
	selected := logs.Containers[0]
	return selected.NodeID + "/" + selected.Container
}

func logOutputFinished(run stepmodel.RunSummary, path string, meta logcontract.Meta) bool {
	return logStepFinished(run, path) && !meta.Copying && (!meta.Follow || logRunFinished(run))
}

func (s *flowSession) announceRunningLog(logs apicontract.LogResult, run stepmodel.RunSummary, path string, cursor *logCursor) error {
	if logs.Content != "" {
		cursor.announced = true
	}
	if cursor.announced || logRunFinished(run) {
		return nil
	}
	for _, step := range run.Steps {
		if step.Path == path && step.Status == stepmodel.StepRunning {
			cursor.announced = true
			return s.fact("Step is running; output follows here.")
		}
	}
	return nil
}
