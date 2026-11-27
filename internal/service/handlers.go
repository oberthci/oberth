package service

import (
	"context"
	"reflect"
	"strconv"
	"time"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func (s *Service) PrepareRunGet(ctx context.Context, c apicontract.Caller, q apicontract.RunGetArgs) (apicontract.Call[apicontract.RunRecord], error) {
	if q.View != "" && q.View != "summary" && q.View != "full" && q.View != "dashboard" {
		return nil, bad("view")
	}
	call, err := s.prepareRunGet(ctx, c.Actor, runQuery{ID: q.ID, Offset: q.StepOffset, Limit: q.StepLimit, Helpers: q.View == "full"})
	return call, err
}
func (s *Service) PrepareRunList(ctx context.Context, c apicontract.Caller, q apicontract.RunListArgs) (apicontract.Call[apicontract.RunPage], error) {
	if q.View != "" && q.View != "all" && q.View != "history" {
		return nil, bad("view")
	}
	call, err := s.prepareRunList(ctx, c.Actor, runQuery{Repo: q.Repo, Before: q.Before, Ref: q.Ref, RefGlob: q.RefGlob, Status: stepmodel.RunStatus(q.Status), Latest: q.Latest, Own: q.Own, Held: q.Held, Limit: q.Limit, Upstreams: q.Upstreams})
	return call, err
}
func (s *Service) PrepareStatus(ctx context.Context, c apicontract.Caller, q apicontract.StatusArgs) (apicontract.Call[stepmodel.RunSummary], error) {
	call, err := s.prepareStatus(ctx, c.Actor, runQuery{Repo: q.Repo, Ref: q.Ref})
	return call, err
}
func (s *Service) PrepareRepoList(ctx context.Context, c apicontract.Caller, q apicontract.RepoListArgs) (apicontract.Call[apicontract.RepoPage], error) {
	if q.Archived != "" && q.Archived != "exclude" && q.Archived != "include" && q.Archived != "only" {
		return nil, bad("archived")
	}
	a, err := s.allow(ctx, c.Actor, "repo_list")
	if err != nil {
		return nil, err
	}
	if _, err := s.listRepositories(ctx, a, runQuery{Repo: q.Repo}); err != nil {
		return nil, err
	}
	return bounded(func(ctx context.Context) (apicontract.RepoPage, error) {
		return authorizedRead(ctx, s, readAuthority{a, "repo_list"}, func(ctx context.Context, a actor.Actor) (apicontract.RepoPage, error) {
			scope, err := s.listRepositories(ctx, a, runQuery{Repo: q.Repo})
			if err != nil {
				return apicontract.RepoPage{}, err
			}
			rows, err := s.repos(ctx, a, scope, q)
			if err != nil {
				return apicontract.RepoPage{}, err
			}
			upstreams, err := s.scopedRepoUpstreams(ctx, a)
			if err != nil {
				return apicontract.RepoPage{}, err
			}
			return apicontract.RepoPage{Repositories: rows, Upstreams: upstreams}, nil
		})
	}), nil
}
func (s *Service) PrepareRunCancel(ctx context.Context, c apicontract.Caller, q apicontract.RunCancelArgs) (apicontract.Call[apicontract.Receipt], error) {
	call, err := s.prepareCancel(ctx, c.Actor, runAction{ID: q.ID, Force: q.Force, Reason: q.Reason})
	return call, err
}
func (s *Service) PrepareRunRerun(ctx context.Context, c apicontract.Caller, q apicontract.RunRerunArgs) (apicontract.Call[apicontract.Receipt], error) {
	call, err := s.prepareRerun(ctx, c.Actor, runAction{ID: q.ID, Assist: q.Assist, Reason: q.Reason})
	return call, err
}
func (s *Service) PrepareRunPurge(ctx context.Context, c apicontract.Caller, q apicontract.RunPurgeArgs) (apicontract.Call[apicontract.Receipt], error) {
	call, err := s.preparePurge(ctx, c.Actor, runAction{ID: q.ID, What: q.What, Reason: q.Reason})
	return call, err
}
func (s *Service) PrepareRunCheck(ctx context.Context, c apicontract.Caller, q apicontract.RunCheckArgs) (apicontract.Call[apicontract.RunCheckResult], error) {
	if q.ReleaseTag != "" && !IsReleaseTag("refs/tags/"+q.ReleaseTag) {
		return nil, bad("release_tag")
	}
	call, err := s.prepareCheck(ctx, c.Actor, CheckRequest{ReleaseTag: q.ReleaseTag, Request: model.CIRequest{Repo: q.Repo, Ref: q.Ref}, Files: q.Files})
	return call, err
}
func (s *Service) PrepareRunWait(ctx context.Context, c apicontract.Caller, q apicontract.RunWaitArgs) (apicontract.Call[apicontract.WaitResult], error) {
	timeout := toolTimeout(q.TimeoutS)

	call, err := s.prepareWait(ctx, c.Actor, runQuery{ID: q.ID, Until: q.Until, Action: "run_wait"}, timeout)
	return call, err
}
func (s *Service) PrepareWait(ctx context.Context, c apicontract.Caller, q apicontract.WaitArgs) (apicontract.Call[apicontract.WaitResult], error) {
	timeout := toolTimeout(q.TimeoutS)

	call, err := s.prepareWait(ctx, c.Actor, runQuery{ID: q.ID, SHA: q.SHA, Ref: q.Ref, Repo: q.Repo, Trigger: model.RunTrigger(q.Trigger), Action: "wait"}, timeout)
	return call, err
}
func (s *Service) PrepareEventsWait(ctx context.Context, c apicontract.Caller, q apicontract.EventsWaitArgs) (apicontract.Call[apicontract.EventResult], error) {
	timeout := toolTimeout(q.TimeoutS)

	input := eventsInput(q)
	input.RunIDs = q.RunIDs
	input.Timeout = timeout
	call, err := s.prepareEventsWait(ctx, c.Actor, input)
	return call, err
}
func (s *Service) PrepareSyncStatus(ctx context.Context, c apicontract.Caller, q apicontract.SyncStatusArgs) (apicontract.Call[apicontract.SyncStatus], error) {
	call, err := s.prepareSyncStatus(ctx, c.Actor)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (apicontract.SyncStatus, error) {
		rows, err := call(ctx)
		return lockResult(rows, q.Lock), err
	}, nil
}
func eventsInput(q apicontract.EventsWaitArgs) eventQuery {
	out := eventQuery{Future: q.AfterID == nil, Snapshot: q.Snapshot, Limit: q.Limit, Own: q.Own, RefsGlob: q.RefsGlob, Repos: q.Repos}
	if q.AfterID != nil {
		out.After = *q.AfterID
	}
	for _, k := range q.Kinds {
		out.Kinds = append(out.Kinds, model.EventKind(k))
	}
	return out
}
func lockResult(rows []Lock, name string) apicontract.SyncStatus {
	out := apicontract.SyncStatus{Locks: []apicontract.LockState{}}
	for _, r := range rows {
		if name != "" && r.Name != name {
			continue
		}
		i := 0
		for i < len(out.Locks) && out.Locks[i].Lock != r.Name {
			i++
		}
		if i == len(out.Locks) {
			var limit *int
			if r.Limit >= 0 {
				value := r.Limit
				limit = &value
			}
			out.Locks = append(out.Locks, apicontract.LockState{Lock: r.Name, Holders: []apicontract.LockHolder{}, Held: r.Held, Limit: limit, Waiters: []string{}})
		}
		switch {
		case r.Clear != "":
			if r.RunID != "" {
				out.Locks[i].Stale = append(out.Locks[i].Stale, r.RunID)
			}
			out.Locks[i].Remedy, out.Locks[i].Command = remedyStaleLock, r.Clear
		case r.Waiting:
			out.Locks[i].Waiters = append(out.Locks[i].Waiters, r.RunID)
		default:
			out.Locks[i].Holders = append(out.Locks[i].Holders, apicontract.LockHolder{RunID: r.RunID, Permits: r.Permits, Steps: r.Steps})
		}
	}
	return out
}

func toolTimeout(seconds int) time.Duration {
	if seconds == 0 {
		field, _ := reflect.TypeFor[apicontract.RunWaitArgs]().FieldByName("TimeoutS")
		seconds, _ = strconv.Atoi(field.Tag.Get("default"))
	}
	return time.Duration(seconds) * time.Second
}

func selectArchivedRepositories(rows []stateport.Repository, archived string) []stateport.Repository {
	selected := []stateport.Repository{}
	for _, r := range rows {
		if archived == "only" && !r.Archived || (archived == "" || archived == "exclude") && r.Archived {
			continue
		}
		selected = append(selected, r)
	}
	return selected
}
