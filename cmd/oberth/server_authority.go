package main

import (
	"context"
	"slices"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
)

type currentAuthority struct{ owner *serverOwners }

func (a currentAuthority) byFingerprint(ctx context.Context, fingerprint, method string) (actor.Actor, error) {
	lookup := a.owner.c.store.Key
	if method == "ssh" {
		lookup = a.owner.c.store.SSHKey
	}
	k, err := lookup(ctx, fingerprint)
	if err != nil {
		return actor.Actor{}, err
	}
	if k.Revoked {
		return actor.Actor{}, publicerr.New(publicerr.Forbidden, "Identity is revoked.", "Use an active identity.", nil)
	}
	return actor.Actor{Identity: k.Identity, Fingerprint: k.Fingerprint, AuthMethod: method, Admin: k.Admin, Scope: slices.Clone(k.Scope)}, nil
}
func (a currentAuthority) Current(ctx context.Context, who actor.Actor) (actor.Actor, error) {
	current, err := a.byFingerprint(ctx, who.Fingerprint, who.AuthMethod)
	if err == nil && current.Identity != who.Identity {
		return actor.Actor{}, publicerr.New(publicerr.Forbidden, "Identity binding changed.", "Use the current identity.", nil)
	}
	return current, err
}
func (a currentAuthority) KeyPolicy(ctx context.Context, who actor.Actor) (service.KeyPolicy, error) {
	k, err := a.owner.c.store.Key(ctx, who.Fingerprint)
	return service.KeyPolicy{Agent: k.Kind == "agent" || k.ParkOnPush}, err
}
type authorityPublication struct{ owner *serverOwners }

func (p authorityPublication) Publish(ctx context.Context, repository string, apply func(context.Context) (stateport.AuthoritySnapshot, error)) (unsynced, err error) {
	return p.owner.c.schedules.AuthorityChanged(ctx, repository, func(ctx context.Context) error { _, err := apply(ctx); return err })
}
