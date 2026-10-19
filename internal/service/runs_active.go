package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

// ActiveRun is one entry of the active run view: the run record, with the
// same capitalized fields as an /api/runs entry, plus its current scheduling
// observation while it has not started. A started run carries none; its
// detail (run_get, /api/runs/{run}) reads the execution observation.
type ActiveRun struct {
	model.Run
	Scheduling *model.RunScheduling `json:"Scheduling,omitempty"`
}

// activeRuns lists every non-terminal run in scheduling order: started runs
// (oldest start first) and admitted runs awaiting submission, then claimed
// runs in admission order (preparing, or waiting on weight, group, VM
// capacity or disk), then unclaimed queued runs in accepted order. The list
// is bounded by store.MaxActiveRuns and never paged.
func (service *API) activeRuns(ctx context.Context, filter api.RunFilter) (any, error) {
	if filter.Before != 0 || strings.TrimSpace(filter.BeforeRun) != "" {
		return nil, fmt.Errorf("%w: before does not apply to the active view, which is not paged", ErrInvalidInput)
	}
	storeFilter := model.RunListFilter{Ref: filter.Ref}
	if strings.TrimSpace(filter.Repository) != "" {
		repository, err := service.runs.RepositoryByName(ctx, filter.Repository)
		if err != nil {
			return nil, err
		}
		storeFilter.RepoID = repository.ID
	}
	runs, err := service.history.ListActiveRuns(ctx, storeFilter)
	if err != nil {
		return nil, err
	}
	// The list observes admission only: one Workflow and Pod read per
	// started run would be unbounded work on every dashboard poll.
	observer := *service
	observer.schedulingObserver = nil
	started := make([]ActiveRun, 0, len(runs))
	admitted := make([]ActiveRun, 0)
	pending := make([]ActiveRun, 0, len(runs))
	for _, run := range runs {
		entry := ActiveRun{Run: visibleRun(run)}
		if run.StartedAt != nil {
			started = append(started, entry)
			continue
		}
		// A run not yet started has no Workflow, so no step results.
		entry.Scheduling = observer.scheduling(ctx, run, nil)
		if run.Status == model.RunRunning && entry.Scheduling != nil &&
			entry.Scheduling.Admission != nil && entry.Scheduling.Admission.State == "admitted" {
			admitted = append(admitted, entry)
			continue
		}
		pending = append(pending, entry)
	}
	active := append(append(started, admitted...), pending...)
	if service.scope != nil {
		for index := range active {
			active[index].QueueSequence = 0
		}
	}
	return active, nil
}
