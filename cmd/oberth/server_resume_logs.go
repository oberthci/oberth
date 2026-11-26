package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stateport"
	"slices"
)

func (l serverLogs) failedAttemptLog(ctx context.Context, r stateport.Run, q apicontract.RunLogsArgs) (stateport.Run, runlog.ReadRequest, bool, error) {
	request := logRequest(q)
	next, err := l.owner.c.store.ResumeSuccessor(ctx, r.ID)
	if err != nil || next == "" {
		return r, request, false, err
	}
	target, err := l.owner.c.store.Run(ctx, next)
	if err != nil {
		return r, request, false, err
	}
	var plan nativebinding.ResumePlan
	if err = json.Unmarshal(target.Origin.ResumePlan, &plan); err != nil {
		return r, request, false, err
	}
	h := plan.InPod
	if !failedAttemptSelected(r, q, h) {
		return r, request, false, nil
	}
	request.Selector.Step, request.Selector.StepSet = h.Step, true
	request.Selector.NodeID, request.Selector.Container = h.NodeID, "main"
	request.PlannedSteps = []string{h.Step}
	request.BeforeMarker = nativebinding.RerunMarker(h.Source.Revision.Commit, h.RunID)
	request.AfterMarker, err = previousAttemptMarker(r, *h)
	if err != nil {
		return r, request, false, err
	}
	target, err = l.failedAttemptArchive(ctx, target, *h)
	return target, request, true, err
}

func previousAttemptMarker(r stateport.Run, h nativebinding.ResumeHold) (string, error) {
	if !r.Origin.Resumed() {
		return "", nil
	}
	var previous nativebinding.ResumePlan
	if err := json.Unmarshal(r.Origin.ResumePlan, &previous); err != nil {
		return "", err
	}
	if p := previous.InPod; p != nil && p.PodUID == h.PodUID {
		return nativebinding.RerunMarker(p.Source.Revision.Commit, p.RunID), nil
	}
	return "", nil
}

func failedAttemptSelected(r stateport.Run, q apicontract.RunLogsArgs, h *nativebinding.ResumeHold) bool {
	if h == nil || q.Containers {
		return false
	}
	if q.Step != "" && carriedStepPath(r, q.Step) != h.Step {
		return false
	}
	if q.NodeID != "" && q.NodeID != h.NodeID {
		return false
	}
	return q.Container == "" || q.Container == "main"
}

func (l serverLogs) failedAttemptArchive(ctx context.Context, target stateport.Run, h nativebinding.ResumeHold) (stateport.Run, error) {
	// Repeated in-Pod handoffs archive the same physical log only when the
	// container finally exits. Resolve that location, retaining R1's boundary.
	seen := map[string]bool{}
	for {
		if seen[target.ID] {
			return target, fmt.Errorf("resume log cycle")
		}
		seen[target.ID] = true
		next, err := l.owner.c.store.ResumeSuccessor(ctx, target.ID)
		if err != nil {
			return target, err
		}
		if next == "" {
			break
		}
		candidate, err := l.owner.c.store.Run(ctx, next)
		if err != nil {
			return target, err
		}
		var p nativebinding.ResumePlan
		if err = json.Unmarshal(candidate.Origin.ResumePlan, &p); err != nil {
			return target, err
		}
		if p.InPod == nil || p.InPod.PodUID != h.PodUID || p.InPod.NodeID != h.NodeID {
			break
		}
		target = candidate
	}
	return target, nil
}

func (l serverLogs) carriedLogs(ctx context.Context, r stateport.Run, q apicontract.RunLogsArgs) (stateport.Run, bool, error) {
	if !r.Origin.Resumed() || q.Step == "" {
		return r, false, nil
	}
	var plan nativebinding.ResumePlan
	if err := json.Unmarshal(r.Origin.ResumePlan, &plan); err != nil {
		return r, false, err
	}
	path := carriedStepPath(r, q.Step)
	if !slices.Contains(plan.Carried, path) {
		return r, false, nil
	}
	old, err := l.owner.c.store.Run(ctx, r.Origin.ResumedFrom)
	if err != nil {
		return r, false, err
	}
	if old.Repo != r.Repo {
		return r, false, publicerr.New(publicerr.NotFound, "Carried logs are unavailable.", "Inspect the run's recorded resume lineage.", nil)
	}
	return old, true, nil
}

func carriedStepPath(r stateport.Run, selector string) string {
	path, matches := selector, 0
	for _, entry := range r.Admission.Plan.Entries {
		if entry.Path == selector {
			return selector
		}
		if entry.Name == selector {
			path = entry.Path
			matches++
		}
	}
	if matches > 1 {
		return selector
	}
	return path
}
