package service

import (
	"context"
	"errors"
	"slices"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
)

// A long wait never delivers a view prepared under a different scope. A fresh
// attachment resolves its selectors again under the current authority.
func (s *Service) refreshEventScope(ctx context.Context, a actor.Actor, q eventQuery) (actor.Actor, eventQuery, error) {
	current, err := s.allow(ctx, a, "events_wait")
	if err != nil {
		return current, q, err
	}
	if !slices.Equal(a.Scope, current.Scope) {
		return current, q, refuse(forbidden)
	}
	q, err = s.rebindEventScope(ctx, current, q)
	return current, q, err
}

func (s *Service) rebindEventScope(ctx context.Context, current actor.Actor, q eventQuery) (eventQuery, error) {
	for _, repo := range append(slices.Clone(q.Repos), q.runRepositories...) {
		if _, err := s.resolveRepo(ctx, current, repo); err != nil {
			return q, err
		}
	}
	var err error
	if len(q.Repos) == 0 {
		q.repositories, err = s.repositoryScope(ctx, current)
	}
	return q, err
}

func (s *Service) eventTimeout(ctx context.Context, a actor.Actor, q eventQuery, cause error) (apicontract.EventResult, error) {
	if ctx.Err() != nil {
		return apicontract.EventResult{}, ctx.Err()
	}
	if !errors.Is(cause, context.DeadlineExceeded) {
		return apicontract.EventResult{}, cause
	}
	a, q, err := s.refreshEventScope(ctx, a, q)
	if err != nil {
		return apicontract.EventResult{}, err
	}
	return authorizedRead(ctx, s, readAuthority{a, "events_wait"}, func(ctx context.Context, current actor.Actor) (apicontract.EventResult, error) {
		page, err := s.eventsSince(ctx, current, q)
		return apicontract.EventResult{EventPage: page, TimedOut: true}, err
	})
}
