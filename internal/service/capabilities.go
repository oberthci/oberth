package service

import (
	"context"
	"encoding/json"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stateport"
)

func (s *Service) PrepareWorkflowCapabilityGet(ctx context.Context, c apicontract.Caller, q apicontract.WorkflowCapabilityGetArgs) (apicontract.Call[apicontract.Capability], error) {
	a, e := s.peopleAllow(ctx, c.Actor, "workflow_capability_get")
	if e != nil {
		return nil, e
	}
	r, e := s.resolveRepo(ctx, a, q.Repo)
	if e != nil {
		return nil, e
	}
	return bounded(func(ctx context.Context) (apicontract.Capability, error) {
		v, e := s.c.People.Grants.Grants(ctx, []string{r.Canonical})
		if e != nil {
			return apicontract.Capability{}, e
		}
		for _, g := range v.Grants {
			if g.Kind == stateport.GrantCapability && !g.Revoke {
				var out apicontract.Capability
				e = json.Unmarshal(g.Capability, &out)
				return out, e
			}
		}
		return apicontract.Capability{}, refuse(missing)
	}), nil
}
func (s *Service) PrepareWorkflowCapabilityGrant(ctx context.Context, c apicontract.Caller, q apicontract.WorkflowCapabilityGrantArgs) (apicontract.Call[apicontract.Capability], error) {
	a, e := s.peopleAllow(ctx, c.Actor, "workflow_capability_grant")
	if e != nil {
		return nil, e
	}
	r, e := s.resolveRepo(ctx, a, q.Repo)
	if e != nil {
		return nil, e
	}
	v, e := s.capability(ctx, q.Capability)
	if e != nil {
		return nil, e
	}
	c.Actor = a
	return bounded(func(ctx context.Context) (apicontract.Capability, error) {
		_, err := s.changeGrant(ctx, c, stateport.GrantChange{Kind: stateport.GrantCapability, Repository: r.Canonical, Capability: capabilityJSON(v)}, q.Reason)
		return v, err
	}), nil
}
func (s *Service) PrepareWorkflowCapabilityRevoke(ctx context.Context, c apicontract.Caller, q apicontract.WorkflowCapabilityRevokeArgs) (apicontract.Call[apicontract.Receipt], error) {
	a, err := s.peopleAllow(ctx, c.Actor, "workflow_capability_revoke")
	if err != nil {
		return nil, err
	}
	r, err := s.resolveRepo(ctx, a, q.Repo)
	if err != nil {
		return nil, err
	}
	c.Actor = a
	return bounded(func(ctx context.Context) (apicontract.Receipt, error) {
		incomplete, err := s.changeGrant(ctx, c, stateport.GrantChange{Kind: stateport.GrantCapability, Repository: r.Canonical, Revoke: true}, q.Reason)
		out := apicontract.Receipt{ID: r.Canonical, State: "revoked"}
		if incomplete {
			out.Warning = warningAuthorityIncomplete
		}
		return out, err
	}), nil
}
