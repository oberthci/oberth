package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

// heldRunsReadBudget bounds the Pod listing behind run_list held:true.
const heldRunsReadBudget = 10 * time.Second

// heldRuns is run_list held:true: the running assisted runs that hold at
// least one parked retry attempt no hold action released, in scheduling
// order, as one unpaged list (the active runs are bounded). own, repo, ref
// and status narrow it like the other views. A failed Pod listing is an
// error, never "no holds".
func (service *API) heldRuns(ctx context.Context, filter api.RunFilter) (any, error) {
	if filter.Before != 0 || strings.TrimSpace(filter.BeforeRun) != "" {
		return nil, fmt.Errorf("%w: before does not apply to held:true, which is one unpaged list", ErrInvalidInput)
	}
	if !api.ValidRunView(filter.View) {
		return nil, fmt.Errorf("%w: view must be all, active or history", ErrInvalidInput)
	}
	if filter.View == api.RunViewHistory {
		return nil, fmt.Errorf("%w: held:true lists running runs; it cannot combine with view history", ErrInvalidInput)
	}
	if service.history == nil {
		return nil, refuse(ErrUnavailable, "run history")
	}
	if err := filter.Status.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	if service.assistHolds == nil || !service.assistHolds.AssistPolicy().Enabled {
		return nil, refuse(ErrUnavailable, "assisted continuation")
	}
	storeFilter := model.RunListFilter{Status: filter.Status, Ref: filter.Ref, Actor: filter.Actor}
	if strings.TrimSpace(filter.Repository) != "" {
		repository, err := service.runs.RepositoryByName(ctx, filter.Repository)
		if err != nil {
			return nil, err
		}
		storeFilter.RepoID = repository.ID
	}
	active, err := service.history.ListActiveRuns(ctx, storeFilter)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, heldRunsReadBudget)
	defer cancel()
	held := make([]model.Run, 0)
	for _, candidate := range active {
		if candidate.Status != model.RunRunning || !candidate.AssistRequested || candidate.WorkflowName == "" {
			continue
		}
		// The list row may omit the assist record; the full row says which
		// holds an action already released.
		run, err := service.history.Run(ctx, candidate.ID)
		if err != nil {
			return nil, err
		}
		holds, err := service.assistHolds.ListAssistHolds(ctx, run.WorkflowName)
		if err != nil {
			return nil, fmt.Errorf("list the holds of run %s: %w", shortRunID(run.ID), err)
		}
		for _, hold := range holds {
			if !hold.Released && !assistReleased(run.Assist, hold.Pod) {
				candidate.QueuePosition = 0
				held = append(held, candidate)
				break
			}
		}
	}
	held, err = service.presentationRuns(ctx, held)
	if err != nil {
		return nil, err
	}
	return service.stripScopedSequences(held), nil
}
