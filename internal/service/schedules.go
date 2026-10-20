package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// SchedulePauseView says who paused a schedule entry, when and why.
type SchedulePauseView struct {
	By     string `json:"by"`
	At     string `json:"at"`
	Reason string `json:"reason,omitempty"`
}

// ScheduleEntryView is one entry of a repository's .oberth/schedule.yaml with
// its durable state.
type ScheduleEntryView struct {
	Name        string             `json:"name"`
	Cron        string             `json:"cron"`
	Branch      string             `json:"branch,omitempty"`
	NextAt      string             `json:"next_at,omitempty"`
	LastFiredAt string             `json:"last_fired_at,omitempty"`
	LastOutcome string             `json:"last_outcome,omitempty"`
	Paused      *SchedulePauseView `json:"paused,omitempty"`
}

// ScheduleRefusalView is an entry the scheduler refuses to run.
type ScheduleRefusalView struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// ScheduleRepoView is one repository's schedule. Repo is the qualified
// upstream/org/repo name. Error is set when the whole file is refused.
type ScheduleRepoView struct {
	Repo          string                `json:"repo"`
	DefaultBranch string                `json:"default_branch"`
	Entries       []ScheduleEntryView   `json:"entries"`
	Refused       []ScheduleRefusalView `json:"refused,omitempty"`
	Error         string                `json:"error,omitempty"`
}

// ScheduleListResponse is the schedule_list result.
type ScheduleListResponse struct {
	Schedules []ScheduleRepoView `json:"schedules"`
}

// SchedulePlan is a resolved manual trigger: the entry, the branch and the
// commit the run would test.
type SchedulePlan struct {
	Repository model.Repository
	Qualified  string
	Entry      string
	Branch     string
	SHA        string
}

// ScheduleTriggerResponse is the schedule_trigger result.
type ScheduleTriggerResponse struct {
	Repo   string `json:"repo"`
	Entry  string `json:"entry"`
	RunID  string `json:"run_id"`
	Branch string `json:"branch"`
	SHA    string `json:"sha"`
	Status string `json:"status"`
}

// ScheduleControl is the scheduler's read and trigger surface (#1106). The
// scheduler owns the schedule file, its parse limits and the busy-repository
// rule, so the tools ask it instead of reading Git themselves.
type ScheduleControl interface {
	// InspectSchedule reports the repository's schedule; ok is false when it
	// has none.
	InspectSchedule(ctx context.Context, repository model.Repository) (view ScheduleRepoView, ok bool, err error)
	// PlanScheduleTrigger resolves one entry to the branch head it would
	// test. It refuses a repository with active runs unless force is set.
	PlanScheduleTrigger(ctx context.Context, repository model.Repository, entry string, force bool) (SchedulePlan, error)
	// EnqueueSchedule queues the planned run through the same admission path
	// as a cron fire, attributed to actor.
	EnqueueSchedule(ctx context.Context, plan SchedulePlan, actor string) (model.EnqueueRunResult, error)
}

// scheduleList serves schedule_list: any uplink may read, a scoped uplink sees
// the schedules of its own repositories.
func (service *API) scheduleList(ctx context.Context, raw json.RawMessage) (any, error) {
	var arguments struct {
		Repo string `json:"repo"`
	}
	if err := decodeTool(raw, &arguments); err != nil {
		return nil, err
	}
	if service.schedules == nil {
		return nil, fmt.Errorf("%w: schedules", ErrUnavailable)
	}
	var repositories []model.Repository
	if strings.TrimSpace(arguments.Repo) != "" {
		repository, err := service.runs.RepositoryByName(ctx, arguments.Repo)
		if err != nil {
			return nil, err
		}
		repositories = []model.Repository{repository}
	} else {
		var err error
		if service.repositories != nil {
			repositories, err = service.repositories.ListRepositories(ctx)
		} else {
			repositories, err = service.runs.ListRepositories(ctx)
		}
		if err != nil {
			return nil, err
		}
	}
	response := ScheduleListResponse{Schedules: []ScheduleRepoView{}}
	for _, repository := range repositories {
		view, ok, err := service.schedules.InspectSchedule(ctx, repository)
		if err != nil {
			return nil, err
		}
		if ok {
			response.Schedules = append(response.Schedules, view)
		}
	}
	return response, nil
}

// scheduleEntry resolves the repository and entry of an admin schedule
// mutation. The caller must be an admin; a scoped admin reaches only its own
// repositories, and a hidden repository answers as a missing one.
func (service *API) scheduleEntry(ctx context.Context, actor api.Actor, tool, repo, entry string) (model.Repository, ScheduleRepoView, ScheduleEntryView, error) {
	if !actor.Admin {
		return model.Repository{}, ScheduleRepoView{}, ScheduleEntryView{}, fmt.Errorf("%w: %s requires an admin uplink", ErrForbidden, tool)
	}
	if service.schedules == nil || service.auditor == nil {
		return model.Repository{}, ScheduleRepoView{}, ScheduleEntryView{}, fmt.Errorf("%w: schedules", ErrUnavailable)
	}
	if strings.TrimSpace(repo) == "" || strings.TrimSpace(entry) == "" {
		return model.Repository{}, ScheduleRepoView{}, ScheduleEntryView{}, fmt.Errorf("%w: repo and entry are required", ErrInvalidInput)
	}
	repository, err := service.runs.RepositoryByName(ctx, repo)
	if err != nil {
		return model.Repository{}, ScheduleRepoView{}, ScheduleEntryView{}, err
	}
	view, ok, err := service.schedules.InspectSchedule(ctx, repository)
	if err != nil {
		return model.Repository{}, ScheduleRepoView{}, ScheduleEntryView{}, err
	}
	if ok {
		for _, candidate := range view.Entries {
			if candidate.Name == entry {
				return repository, view, candidate, nil
			}
		}
	}
	return model.Repository{}, ScheduleRepoView{}, ScheduleEntryView{}, fmt.Errorf("%w: %s has no runnable schedule entry %q", ErrInvalidInput, repo, entry)
}

// schedulePauseResume serves schedule_pause and schedule_resume. The audit
// action is the durable pause state (see store.SchedulePauses), so it is
// written before the answer and never skipped.
func (service *API) schedulePauseResume(ctx context.Context, actor api.Actor, pause bool, raw json.RawMessage) (any, error) {
	tool := "schedule_resume"
	if pause {
		tool = "schedule_pause"
	}
	var arguments struct {
		Repo   string `json:"repo"`
		Entry  string `json:"entry"`
		Reason string `json:"reason"`
	}
	if err := decodeTool(raw, &arguments); err != nil {
		return nil, err
	}
	if !actor.Admin {
		return nil, fmt.Errorf("%w: %s requires an admin uplink", ErrForbidden, tool)
	}
	repository, view, entry, err := service.scheduleEntry(ctx, actor, tool, arguments.Repo, arguments.Entry)
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(arguments.Reason)
	if len(reason) > 512 {
		return nil, fmt.Errorf("%w: reason is longer than 512 bytes", ErrInvalidInput)
	}
	if (entry.Paused != nil) == pause {
		// Already in the requested state: report it without a second audit row.
		return map[string]any{"repo": view.Repo, "entry": entry.Name, "paused": entry.Paused != nil, "changed": false, "state": entry}, nil
	}
	action := "schedule.resume"
	details := map[string]any{"repo": view.Repo, "entry": entry.Name}
	if pause {
		action = "schedule.pause"
		details["reason"] = reason
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return nil, err
	}
	if _, err := service.auditor.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: actor.Identity, Action: action, ResourceType: "schedule",
		ResourceID: store.ScheduleAuditResourceID(view.Repo, entry.Name), Details: string(encoded),
	}); err != nil {
		return nil, fmt.Errorf("audit %s: %w", action, err)
	}
	refreshed, ok, err := service.schedules.InspectSchedule(ctx, repository)
	state := entry
	if err == nil && ok {
		for _, candidate := range refreshed.Entries {
			if candidate.Name == entry.Name {
				state = candidate
			}
		}
	}
	return map[string]any{"repo": view.Repo, "entry": entry.Name, "paused": pause, "changed": true, "state": state}, nil
}

// scheduleTrigger serves schedule_trigger: it queues the entry's run now,
// through the scheduler's own EnqueueCI path (the same one a cron fire and a
// push use), with trigger "schedule" and the caller as actor. The audit action
// is written first, so a failed audit queues nothing.
func (service *API) scheduleTrigger(ctx context.Context, actor api.Actor, raw json.RawMessage) (any, error) {
	var arguments struct {
		Repo  string `json:"repo"`
		Entry string `json:"entry"`
		Force bool   `json:"force"`
	}
	if err := decodeTool(raw, &arguments); err != nil {
		return nil, err
	}
	repository, view, entry, err := service.scheduleEntry(ctx, actor, "schedule_trigger", arguments.Repo, arguments.Entry)
	if err != nil {
		return nil, err
	}
	if entry.Paused != nil && !arguments.Force {
		return nil, fmt.Errorf("%w: entry %q is paused by %s; resume it, or pass force to run it once anyway", ErrInvalidInput, entry.Name, entry.Paused.By)
	}
	plan, err := service.schedules.PlanScheduleTrigger(ctx, repository, entry.Name, arguments.Force)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(map[string]any{"repo": view.Repo, "entry": entry.Name, "branch": plan.Branch, "sha": plan.SHA, "force": arguments.Force})
	if err != nil {
		return nil, err
	}
	if _, err := service.auditor.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: actor.Identity, Action: "schedule.trigger", ResourceType: "schedule",
		ResourceID: store.ScheduleAuditResourceID(view.Repo, entry.Name), Details: string(encoded),
	}); err != nil {
		return nil, fmt.Errorf("audit schedule.trigger: %w", err)
	}
	enqueued, err := service.schedules.EnqueueSchedule(ctx, plan, actor.Identity)
	if err != nil {
		return nil, err
	}
	return ScheduleTriggerResponse{
		Repo: view.Repo, Entry: entry.Name, RunID: enqueued.ID, Branch: plan.Branch, SHA: plan.SHA, Status: string(enqueued.Status),
	}, nil
}
