package main

import (
	"context"
	"encoding/json"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"slices"
)

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
