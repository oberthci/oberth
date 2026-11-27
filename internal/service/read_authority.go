package service

import (
	"context"
	"slices"

	"github.com/oberthci/oberth/internal/actor"
)

type readAuthority struct {
	actor  actor.Actor
	action string
}

// One page uses one current actor snapshot, then confirms it before delivery.
// A scope change discards the complete cached result, including earlier rows.
func authorizedRead[T any](ctx context.Context, s *Service, authority readAuthority, read func(context.Context, actor.Actor) (T, error)) (T, error) {
	var zero T
	a, err := s.allow(ctx, authority.actor, authority.action)
	if err != nil {
		return zero, err
	}
	value, err := read(ctx, a)
	if err != nil {
		return zero, err
	}
	current, err := s.allow(ctx, a, authority.action)
	if err != nil {
		return zero, err
	}
	if !slices.Equal(current.Scope, a.Scope) {
		return zero, refuse(forbidden)
	}
	return value, nil
}
