package main

import (
	"context"
	"errors"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
)

// Authority changes settle accepted receives before returning their receipt.
type serverPeopleStore struct {
	*store.Store
	owner *serverOwners
}

func (s serverPeopleStore) RevokeKey(ctx context.Context, a stateport.Action, fingerprint string) error {
	return s.settleReceives(ctx, s.Store.RevokeKey(ctx, a, fingerprint))
}
func (s serverPeopleStore) ReplaceKey(ctx context.Context, a stateport.Action, r stateport.KeyReplacement) error {
	return s.settleReceives(ctx, s.Store.ReplaceKey(ctx, a, r))
}
func (s serverPeopleStore) ArchiveRepository(ctx context.Context, a stateport.Action, id int64) error {
	return s.settleReceives(ctx, s.Store.ArchiveRepository(ctx, a, id))
}
func (s serverPeopleStore) RemoveRepository(ctx context.Context, a stateport.Action, id int64) error {
	return s.settleReceives(ctx, s.Store.RemoveRepository(ctx, a, id))
}
func (s serverPeopleStore) settleReceives(ctx context.Context, err error) error {
	if err == nil {
		s.owner.notice(s.owner.abandonUnauthorizedReceives(ctx))
	}
	return err
}

func (o *serverOwners) abandonUnauthorizedReceives(ctx context.Context) error {
	records, err := o.cache.PendingReceives(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, record := range records {
		if record.State != "ready" {
			continue
		}
		err := o.receiveDeliveryAuthority(ctx, record.Event)
		var abandoned *gitcache.AbandonReceiveError
		if errors.As(err, &abandoned) {
			err = o.cache.AbandonReceive(ctx, record.Event.Repository, record.Event.ID)
		}
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func (o *serverOwners) maintainReceives(ctx context.Context) error {
	return errors.Join(o.cache.ReplayPending(ctx, o.deliver), o.cache.GC(ctx))
}
