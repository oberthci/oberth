package main

import (
	"slices"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/logcontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func defaultLogContainer(steps []stepmodel.RunStep, selector logcontract.Selector, entries []runlog.ContainerLog) (logcontract.Selector, []apicontract.Container) {
	if selector.Container != "" {
		return selector, nil
	}
	for _, step := range steps {
		matches := selector.StepSet && selector.Step == step.Path
		if selector.NodeID != "" {
			matches = slices.ContainsFunc(step.Attempts, func(a stepmodel.StepAttempt) bool { return a.NodeID == selector.NodeID })
		}
		if !matches {
			continue
		}
		if failed := failedLogContainer(step, selector.NodeID, entries); failed != nil {
			selector.NodeID, selector.Container = failed.NodeID, failed.Container
			return selector, []apicontract.Container{{NodeID: failed.NodeID, PodName: failed.PodName, Container: failed.Container}}
		}
	}
	return selector, nil
}

func failedLogContainer(step stepmodel.RunStep, node string, entries []runlog.ContainerLog) *stepmodel.LogSelector {
	if step.Status != stepmodel.StepFailed {
		return nil
	}
	for i := len(step.Attempts) - 1; i >= 0; i-- {
		attempt := step.Attempts[i]
		if attempt.Status != stepmodel.StepFailed || node != "" && node != attempt.NodeID {
			continue
		}
		for _, hint := range step.Logs {
			if hint.NodeID == attempt.NodeID && hint.Container != "" && !authoredLogContainer(hint, entries) {
				return &hint
			}
		}
		return nil
	}
	return nil
}

func authoredLogContainer(hint stepmodel.LogSelector, entries []runlog.ContainerLog) bool {
	return slices.ContainsFunc(entries, func(entry runlog.ContainerLog) bool {
		return entry.NodeID == hint.NodeID && entry.Container == hint.Container && entry.Main
	})
}

func endedLogMeta(run stateport.Run, steps []stepmodel.RunStep, selector logcontract.Selector, meta logcontract.Meta) logcontract.Meta {
	if run.Status != stepmodel.RunFailed || meta.Copying {
		return meta
	}
	for _, step := range steps {
		matches := selector.StepSet && selector.Step == step.Path
		if selector.NodeID != "" {
			matches = slices.ContainsFunc(step.Attempts, func(a stepmodel.StepAttempt) bool { return a.NodeID == selector.NodeID })
		}
		if matches && step.Status == stepmodel.StepFailed {
			meta.Reason, meta.Detail, meta.Follow = step.DisplayReason, step.Detail, false
		}
	}
	return meta
}

func defaultLogUnavailable(err error, run stateport.Run, containers []apicontract.Container) bool {
	code := publicerr.Classify(err).Code()
	return code == publicerr.InvalidTransition || code == publicerr.NotFound && run.Status == stepmodel.RunFailed && len(containers) > 0
}
