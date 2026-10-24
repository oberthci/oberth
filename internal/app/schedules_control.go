package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/schedule"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
)

var _ service.ScheduleControl = (*Schedules)(nil)

// readSchedule reads and parses a repository's schedule file at its default
// branch head. found is false when the repository has no schedule file (or no
// readable default branch); a refused file is returned with its error.
func (schedules *Schedules) readSchedule(ctx context.Context, repository model.Repository, now time.Time) (qualified, head string, file schedule.File, found bool, parseErr error, err error) {
	if schedules.config.Git == nil {
		return "", "", schedule.File{}, false, nil, errors.New("schedule reads are unavailable")
	}
	branch := strings.TrimSpace(repository.DefaultBranch)
	if branch == "" {
		return "", "", schedule.File{}, false, nil, nil
	}
	qualified, err = schedules.qualifiedRepoName(ctx, repository)
	if err != nil {
		return "", "", schedule.File{}, false, nil, err
	}
	head, refErr := schedules.config.Git.RefSHA(ctx, qualified, branch)
	if refErr != nil {
		return qualified, "", schedule.File{}, false, nil, nil
	}
	raw, blobErr := schedules.config.Git.ReadBlob(ctx, qualified, head, schedule.FileName, schedule.MaxSourceBytes)
	if blobErr != nil {
		return qualified, head, schedule.File{}, false, nil, nil
	}
	file, parseErr = schedule.Parse(raw, schedule.Limits{
		MinInterval: schedules.config.MinInterval, MaxEntries: schedules.config.MaxEntries,
	}, schedules.observed)
	return qualified, head, file, true, parseErr, nil
}

// InspectSchedule reports a repository's schedule with each entry's next run,
// last recorded outcome and pause (#1106).
func (schedules *Schedules) InspectSchedule(ctx context.Context, repository model.Repository) (service.ScheduleRepoView, bool, error) {
	now := time.Now().UTC()
	qualified, _, file, found, parseErr, err := schedules.readSchedule(ctx, repository, now)
	if err != nil || !found {
		return service.ScheduleRepoView{}, false, err
	}
	view := service.ScheduleRepoView{Repo: qualified, DefaultBranch: repository.DefaultBranch, Entries: []service.ScheduleEntryView{}}
	if parseErr != nil {
		view.Error = parseErr.Error()
		return view, true, nil
	}
	outcomes := map[string]store.ScheduleOutcome{}
	if reader, ok := schedules.config.State.(ScheduleOutcomeReader); ok {
		stored, err := reader.ScheduleOutcomes(ctx, qualified)
		if err != nil {
			return service.ScheduleRepoView{}, false, err
		}
		for _, outcome := range stored {
			outcomes[outcome.Entry] = outcome
		}
	}
	var pauses map[string]model.SchedulePause
	if reader, ok := schedules.config.State.(SchedulePauseReader); ok {
		if pauses, err = reader.SchedulePauses(ctx, qualified); err != nil {
			return service.ScheduleRepoView{}, false, err
		}
	}
	for _, entry := range file.Entries {
		item := service.ScheduleEntryView{Name: entry.Name, Cron: entry.Cron, Branch: entry.Branch}
		if next, nextErr := entry.Next(now); nextErr == nil {
			item.NextAt = next.UTC().Format(time.RFC3339)
		}
		if outcome, ok := outcomes[entry.Name]; ok {
			item.LastOutcome = outcome.Outcome
			item.LastFiredAt = outcome.FiredAt.UTC().Format(time.RFC3339)
		}
		if pause, ok := pauses[entry.Name]; ok {
			item.Paused = &service.SchedulePauseView{By: pause.By, At: pause.At.UTC().Format(time.RFC3339), Reason: pause.Reason}
		}
		view.Entries = append(view.Entries, item)
	}
	for _, refusal := range file.Refused {
		view.Refused = append(view.Refused, service.ScheduleRefusalView{Name: refusal.Name, Reason: refusal.Reason})
	}
	return view, true, nil
}

// PlanScheduleTrigger resolves one entry to the branch head a manual run would
// test, applying the cron path's busy-repository rule unless force is set.
func (schedules *Schedules) PlanScheduleTrigger(ctx context.Context, repository model.Repository, name string, force bool) (service.SchedulePlan, error) {
	qualified, head, file, found, parseErr, err := schedules.readSchedule(ctx, repository, time.Now().UTC())
	if err != nil {
		return service.SchedulePlan{}, err
	}
	if !found || parseErr != nil {
		return service.SchedulePlan{}, fmt.Errorf("%w: %s has no usable schedule file", service.ErrInvalidInput, repository.Name)
	}
	var entry schedule.Entry
	for _, candidate := range file.Entries {
		if candidate.Name == name {
			entry = candidate
			break
		}
	}
	if entry.Name == "" {
		return service.SchedulePlan{}, fmt.Errorf("%w: %s has no runnable schedule entry %q", service.ErrInvalidInput, repository.Name, name)
	}
	if !force && schedules.busy(ctx, repository.ID) {
		return service.SchedulePlan{}, fmt.Errorf("%w: %s has active runs; wait for them, or pass force to queue the run anyway", store.ErrInvalidState, repository.Name)
	}
	branch, sha, err := schedules.entryTarget(ctx, qualified, entry, head, repository.DefaultBranch)
	if err != nil {
		return service.SchedulePlan{}, fmt.Errorf("resolve branch for %q: %w", name, err)
	}
	return service.SchedulePlan{Repository: repository, Qualified: qualified, Entry: name, Branch: branch, SHA: sha}, nil
}

// EnqueueSchedule queues a planned manual run through the scheduler's own
// EnqueueCI, the path every cron fire and push takes. The actor is the
// uplink that asked, not the scheduler.
func (schedules *Schedules) EnqueueSchedule(ctx context.Context, plan service.SchedulePlan, actor string) (model.EnqueueRunResult, error) {
	if schedules.config.Enqueuer == nil {
		return model.EnqueueRunResult{}, errors.New("schedule enqueue is unavailable")
	}
	return schedules.enqueue(ctx, plan.Repository, plan.Branch, plan.SHA, plan.Entry, actor)
}
