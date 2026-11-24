package service

import (
	"context"
	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stateport"
	"slices"
)

func (s *Service) PrepareUplinkList(ctx context.Context, c apicontract.Caller, _ apicontract.UplinkListArgs) (apicontract.Call[apicontract.IdentityPage], error) {
	return prepareAdmin(s, ctx, c, adminOperation[apicontract.IdentityPage]{"uplink_list", func(ctx context.Context, _ actor.Actor) (apicontract.IdentityPage, error) {
		keys, e := s.c.People.Enrollment.IdentityKeys(ctx, "")
		out := apicontract.IdentityPage{Uplinks: []apicontract.Uplink{}}
		for _, k := range keys {
			v := apicontract.Uplink{Identity: k.Identity, Fingerprint: k.Fingerprint, Admin: k.Admin, Scope: publicScope(k.Scope)}
			if k.Revoked {
				v.RevokedAt = k.RevokedAt
			}
			out.Uplinks = append(out.Uplinks, v)
		}
		return out, e
	}})
}
func (s *Service) PrepareUplinkScope(ctx context.Context, c apicontract.Caller, q apicontract.UplinkScopeArgs) (apicontract.Call[apicontract.Identity], error) {
	a, e := s.peopleAllow(ctx, c.Actor, "uplink_scope")
	if e != nil {
		return nil, e
	}
	k, e := s.selectedKey(ctx, q.Identity, q.Fingerprint)
	if e != nil {
		return nil, e
	}
	if q.Scope == nil {
		return nil, bad("scope")
	}
	scope, e := apicontract.ExplicitUplinkScope(scopeNames(q.Scope))
	if e != nil {
		return nil, e
	}
	if e = publicIdentity(k.Identity, scope); e != nil {
		return nil, e
	}
	return bounded(func(ctx context.Context) (apicontract.Identity, error) {
		if _, err := s.requireUpstreams(ctx, scope); err != nil {
			return apicontract.Identity{}, err
		}
		e := s.c.People.Enrollment.SetKeyScope(ctx, s.peopleAction(a, "identity.changed", q.Reason, ""), k.Fingerprint, scope)
		return apicontract.Identity{Identity: k.Identity, Fingerprint: k.Fingerprint, Admin: k.Admin, Scope: scope}, e
	}), nil
}
func (s *Service) PrepareUplinkRevoke(ctx context.Context, c apicontract.Caller, q apicontract.UplinkRevokeArgs) (apicontract.Call[apicontract.Receipt], error) {
	a, e := s.peopleAllow(ctx, c.Actor, "uplink_revoke")
	if e != nil {
		return nil, e
	}
	k, e := s.selectedKey(ctx, q.Identity, q.Fingerprint)
	if e != nil {
		return nil, e
	}
	return bounded(func(ctx context.Context) (apicontract.Receipt, error) {
		e := s.c.People.Enrollment.RevokeKey(ctx, s.peopleAction(a, "identity.changed", q.Reason, ""), k.Fingerprint)
		return apicontract.Receipt{ID: k.Fingerprint, State: "revoked"}, e
	}), nil
}
func (s *Service) PrepareUplinkRotate(ctx context.Context, c apicontract.Caller, q apicontract.UplinkRotateArgs) (apicontract.Call[apicontract.Identity], error) {
	a, e := s.peopleAllow(ctx, c.Actor, "uplink_rotate")
	if e != nil {
		return nil, e
	}
	old, e := s.selectedKey(ctx, q.Identity, q.Fingerprint)
	if e != nil {
		return nil, e
	}
	next, e := s.selectedKey(ctx, q.Identity, q.ReplacementFingerprint)
	if e != nil {
		return nil, e
	}
	if old.Admin != next.Admin || old.Kind != next.Kind || old.ParkOnPush != next.ParkOnPush || !slices.Equal(old.Scope, next.Scope) {
		return nil, refuse(forbidden)
	}
	return bounded(func(ctx context.Context) (apicontract.Identity, error) {
		e := s.c.People.Enrollment.ReplaceKey(ctx, s.peopleAction(a, "identity.changed", q.Reason, ""), stateport.KeyReplacement{Previous: old.Fingerprint, Replacement: next.Fingerprint})
		return apicontract.Identity{Identity: next.Identity, Fingerprint: next.Fingerprint, Admin: next.Admin, Scope: publicScope(next.Scope)}, e
	}), nil
}

func scopeNames(raw *[]*string) []string {
	scope := []string{}
	if raw == nil {
		return scope
	}
	for _, v := range *raw {
		name := ""
		if v != nil {
			name = *v
		}
		scope = append(scope, name)
	}
	return scope
}
