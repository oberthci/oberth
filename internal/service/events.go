package service

import (
	"context"
	"slices"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func validateEventKinds(q eventQuery) error {
	if len(q.Repos) > 32 || len(q.RunIDs) > 32 || len(q.Kinds) > 32 || len(q.RefsGlob) > 32 {
		return bad("filters")
	}
	for _, k := range q.Kinds {
		if !k.Valid() {
			return bad("kinds")
		}
	}
	for _, glob := range q.RefsGlob {
		if !stateport.ValidRefGlob(glob) {
			return bad("refs_glob")
		}
	}
	return nil
}
func (s *Service) prepareEvents(ctx context.Context, a actor.Actor, q eventQuery, name string) (actor.Actor, eventQuery, error) {
	a, err := s.allow(ctx, a, name)
	if err != nil {
		return a, q, err
	}
	if err := validateEventKinds(q); err != nil {
		return a, q, err
	}
	q.Repos = slices.Clone(q.Repos)
	q.RunIDs = slices.Clone(q.RunIDs)
	q.RefsGlob = slices.Clone(q.RefsGlob)
	q.Kinds = slices.Clone(q.Kinds)
	q.runRepositories = nil
	for i, selector := range q.Repos {
		r, err := s.resolveRepo(ctx, a, selector)
		if err != nil {
			return a, q, err
		}
		q.Repos[i] = r.Canonical
	}
	for i, selector := range q.RunIDs {
		r, err := s.resolveRun(ctx, a, runQuery{ID: selector})
		if err != nil {
			return a, q, err
		}
		q.RunIDs[i] = r.ID
		q.runRepositories = append(q.runRepositories, r.Repo)
	}

	if len(q.Repos) > 0 {
		q.repositories = q.Repos
	} else {
		q.repositories, err = s.repositoryScope(ctx, a)
	}
	return a, q, err
}
func (s *Service) eventsSince(ctx context.Context, a actor.Actor, q eventQuery) (model.EventPage, error) {
	scope := q.repositories
	f := stateport.EventFilter{Repositories: scope, Upstreams: issueEventUpstreams(a, q), After: q.After, Limit: pageLimit(q.Limit, model.MaxEventPage), RunIDs: q.RunIDs, Kinds: q.Kinds, RefsGlob: q.RefsGlob}
	if q.Own {
		f.Actor = a.Identity
	}
	page, err := s.c.Events.Events(ctx, f)
	if err != nil {
		return page, err
	}
	return page, nil
}

func (s *Service) prepareEventsWait(ctx context.Context, a actor.Actor, q eventQuery) (func(context.Context) (apicontract.EventResult, error), error) {
	a, q, err := s.prepareEvents(ctx, a, q, "events_wait")
	if err != nil {
		return nil, err
	}
	if q.Future {
		page, err := s.c.Events.Events(ctx, stateport.EventFilter{Repositories: q.repositories, Upstreams: issueEventUpstreams(a, q), Limit: 1})
		if err != nil {
			return nil, err
		}
		q.After = page.LatestID
	}
	if q.Snapshot {
		return func(ctx context.Context) (apicontract.EventResult, error) {
			a, q, err := s.refreshEventScope(ctx, a, q)
			if err != nil {
				return apicontract.EventResult{}, err
			}
			page, err := s.eventsSince(ctx, a, q)
			return apicontract.EventResult{EventPage: page}, err
		}, nil
	}
	return func(ctx context.Context) (apicontract.EventResult, error) { return s.eventsWait(ctx, a, q) }, nil
}

func (s *Service) eventsWait(ctx context.Context, a actor.Actor, q eventQuery) (apicontract.EventResult, error) {
	if err := ctx.Err(); err != nil {
		return apicontract.EventResult{}, err
	}

	timeout := q.Timeout
	if timeout == 0 {
		timeout = toolTimeout(0)
	}
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	authority := s.c.Events.Subscribe(stateport.ChangeScope{})
	defer authority.Close()
	var err error
	for {
		a, q, err = s.refreshEventScope(ctx, a, q)
		if err != nil {
			return apicontract.EventResult{}, err
		}
		sub := s.c.Events.Subscribe(eventChangeScope(a, q))
		page, err := s.eventsSince(ctx, a, q)
		if err != nil {
			sub.Close()
			return apicontract.EventResult{}, err
		}
		result := apicontract.EventResult{EventPage: page}
		if len(page.Events) > 0 || page.Gap || page.HasMore {
			sub.Close()
			if _, _, err := s.refreshEventScope(ctx, a, q); err != nil {
				return apicontract.EventResult{}, err
			}
			return result, nil
		}
		q.After = page.NextAfterID
		err = s.awaitEventChange(wait, sub.C, authority.C)
		sub.Close()
		if err != nil {
			return s.eventTimeout(ctx, a, q, err)
		}
	}
}

func (s *Service) awaitEventChange(ctx context.Context, scoped, authority <-chan struct{}) error {
	select {
	case <-scoped:
		return nil
	case <-authority:
		return nil
	case <-s.ctx.Done():
		return refuse(unavailable)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func eventChangeScope(a actor.Actor, q eventQuery) stateport.ChangeScope {
	if len(q.RunIDs) > 0 {
		return stateport.ChangeScope{Runs: q.RunIDs, Invalidations: q.runRepositories}
	}
	if len(q.Repos) > 0 {
		return stateport.ChangeScope{Repositories: q.Repos}
	}
	return stateport.ChangeScope{Upstreams: a.Scope, All: len(a.Scope) == 0}
}

func (s *Service) prepareSyncStatus(ctx context.Context, a actor.Actor) (func(context.Context) ([]Lock, error), error) {
	a, err := s.allow(ctx, a, "sync_status")
	if err != nil {
		return nil, err
	}
	return bounded(func(ctx context.Context) ([]Lock, error) { return s.syncStatus(ctx, a) }), nil
}

// syncStatus keeps the rows of repositories the caller may touch. A stale row of
// another repository on a lock the caller sees keeps only the lock and the
// command, so a run waiting behind it still learns the remedy.
func (s *Service) syncStatus(ctx context.Context, a actor.Actor) ([]Lock, error) {
	locks, err := s.c.Exec.Locks(ctx)
	out, seen := []Lock{}, map[string]bool{}
	held := map[string]int{}
	for _, l := range locks {
		if !l.Waiting {
			held[l.Name] += l.Permits
		}
	}
	for _, l := range locks {
		if a.CanTouchRepository(l.Repo) {
			l.Held = held[l.Name]
			out, seen[l.Name] = append(out, l), true
		}
	}
	for _, l := range locks {
		if l.Clear != "" && seen[l.Name] && !a.CanTouchRepository(l.Repo) {
			out = append(out, Lock{Name: l.Name, Clear: l.Clear})
		}
	}
	return out, err
}

func issueEventUpstreams(a actor.Actor, q eventQuery) []string {
	if len(q.Repos) > 0 || len(q.RunIDs) > 0 || q.Own {
		return nil
	}
	return a.Scope
}
