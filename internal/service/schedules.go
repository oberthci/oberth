package service

import (
	"context"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stateport"
)

// Schedules is implemented over C10. Mutations commit their audited pause or
// occurrence fact before native effects; Force overrides only the user's pause.
type Schedules interface {
	List(context.Context, []string) (apicontract.SchedulePage, error)
	SetPaused(context.Context, stateport.Action, ScheduleControl, bool) (apicontract.Receipt, error)
	Trigger(context.Context, stateport.Action, ScheduleControl) (apicontract.Receipt, error)
}
type ScheduleControl struct {
	Repository stateport.Repository
	Entry      string
	Force      bool
}
type scheduleAction struct {
	Action, Repo, Entry, Reason string
	Force                       bool
}

func (s *Service) PrepareScheduleList(ctx context.Context, c apicontract.Caller, q apicontract.ScheduleListArgs) (apicontract.Call[apicontract.SchedulePage], error) {
	a, e := s.peopleAllow(ctx, c.Actor, "schedule_list")
	if e != nil {
		return nil, e
	}
	scope, e := s.repositoryScope(ctx, a)
	if e != nil {
		return nil, e
	}
	if q.Repo != "" {
		r, err := s.resolveRepo(ctx, a, q.Repo)
		if err != nil {
			return nil, err
		}
		scope = []string{r.Canonical}
	}
	return bounded(func(ctx context.Context) (apicontract.SchedulePage, error) {
		v, e := s.c.People.Schedules.List(ctx, scope)
		if v.Schedules == nil {
			v.Schedules = []apicontract.Schedule{}
		}
		return v, e
	}), nil
}
func (s *Service) prepareSchedule(ctx context.Context, c apicontract.Caller, q scheduleAction) (apicontract.Call[apicontract.Receipt], error) {
	a, e := s.peopleAllow(ctx, c.Actor, q.Action)
	if e != nil {
		return nil, e
	}
	r, e := s.resolveRepo(ctx, a, q.Repo)
	if e != nil {
		return nil, e
	}
	control := ScheduleControl{Repository: r, Entry: q.Entry, Force: q.Force}
	fact := stateport.Action{Actor: a.Identity, Reason: q.Reason}
	return bounded(func(ctx context.Context) (apicontract.Receipt, error) {
		if q.Action == "schedule_trigger" {
			return s.c.People.Schedules.Trigger(ctx, fact, control)
		}
		return s.c.People.Schedules.SetPaused(ctx, fact, control, q.Action == "schedule_pause")
	}), nil
}
func (s *Service) PrepareSchedulePause(ctx context.Context, c apicontract.Caller, q apicontract.SchedulePauseArgs) (apicontract.Call[apicontract.Receipt], error) {
	return s.prepareSchedule(ctx, c, scheduleAction{Action: "schedule_pause", Repo: q.Repo, Entry: q.Entry, Reason: q.Reason})
}
func (s *Service) PrepareScheduleResume(ctx context.Context, c apicontract.Caller, q apicontract.ScheduleResumeArgs) (apicontract.Call[apicontract.Receipt], error) {
	return s.prepareSchedule(ctx, c, scheduleAction{Action: "schedule_resume", Repo: q.Repo, Entry: q.Entry, Reason: q.Reason})
}
func (s *Service) PrepareScheduleTrigger(ctx context.Context, c apicontract.Caller, q apicontract.ScheduleTriggerArgs) (apicontract.Call[apicontract.Receipt], error) {
	return s.prepareSchedule(ctx, c, scheduleAction{Action: "schedule_trigger", Repo: q.Repo, Entry: q.Entry, Reason: q.Reason, Force: q.Force})
}
func (s *Service) PrepareSystemStatus(ctx context.Context, c apicontract.Caller, _ apicontract.SystemStatusArgs) (apicontract.Call[apicontract.SystemStatus], error) {
	return s.systemStatus(ctx, c, "system_status")
}
