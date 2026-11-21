package service

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"strings"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

func grantInput(q apicontract.AccessAllowArgs) (stateport.GrantChange, error) {
	kind := stateport.GrantKind(q.Kind)
	if kind == "" {
		kind = stateport.GrantStorePath
	}
	if name, missing := apicontract.GrantArgumentProblem(string(kind), grantGiven(q)); name != "" {
		return stateport.GrantChange{}, grantArgument(name, missing)
	}
	g := stateport.GrantChange{Kind: kind, Repository: q.Repo, File: q.Workflow, Step: q.Step, Path: q.Secret, Tier: q.Tier}
	switch kind {
	case stateport.GrantStorePath, stateport.GrantSecret, stateport.GrantConfigMap, stateport.GrantImagePull, stateport.GrantDevice, stateport.GrantAudience:
		if e := grantTuple(q, g); e != nil {
			return g, e
		}
		if kind == stateport.GrantAudience {
			g.Path = q.Audience
		}
	case stateport.GrantPVC:
		g.Path = q.Claim
		if q.Tier != "ci" && q.Tier != "release" {
			return g, bad("tier")
		}
	default:
		return g, bad("kind")
	}
	return g, nil
}

// grantGiven reports which access selectors the request carries.
func grantGiven(q apicontract.AccessAllowArgs) func(string) bool {
	values := map[string]string{"repo": q.Repo, "workflow": q.Workflow, "step": q.Step, "secret": q.Secret, "audience": q.Audience, "claim": q.Claim, "tier": q.Tier}
	return func(name string) bool { return values[name] != "" }
}
func (s *Service) changeGrant(ctx context.Context, c apicontract.Caller, g stateport.GrantChange, reason string) (bool, error) {
	// Revocation commits its exact tuple even if another tuple prevents rebuild.
	// Publish's callback and the resulting generation are protected by C10's fence.
	if g.Revoke || g.Dismiss {
		snapshot, err := s.c.People.Grants.ChangeGrant(ctx, s.peopleAction(c.Actor, "authority.changed", reason, g.Repository), g)
		if err != nil {
			return false, err
		}
		publishErr := s.c.People.Publication.Publish(ctx, func(context.Context) (stateport.AuthoritySnapshot, error) { return snapshot, nil })
		if g.Revoke && g.Kind == stateport.GrantStorePath {
			if err := s.deleteSecret(ctx, c.Actor, g); err != nil {
				return publishErr != nil, err
			}
		}
		return publishErr != nil, nil
	}
	err := s.c.People.Publication.Publish(ctx, func(ctx context.Context) (stateport.AuthoritySnapshot, error) {
		return s.c.People.Grants.ChangeGrant(ctx, s.peopleAction(c.Actor, "authority.changed", reason, g.Repository), g)
	})
	return false, err
}
func (s *Service) prepareAccess(ctx context.Context, c apicontract.Caller, q apicontract.AccessAllowArgs, revoke bool) (apicontract.Call[apicontract.Grant], error) {
	action := "access_allow"
	if revoke {
		action = "access_revoke"
	}
	a, e := s.peopleAllow(ctx, c.Actor, action)
	if e != nil {
		return nil, e
	}
	c.Actor = a
	r, e := s.resolveRepo(ctx, a, q.Repo)
	if e != nil {
		return nil, e
	}
	q.Repo = r.Canonical
	g, e := grantInput(q)
	if e != nil {
		return nil, e
	}
	g.Revoke = revoke
	return bounded(func(ctx context.Context) (apicontract.Grant, error) {
		if g.Kind == stateport.GrantStorePath {
			release, err := s.acquire(ctx, secretKey(g.Repository))
			if err != nil {
				return apicontract.Grant{}, err
			}
			defer release()
		}
		incomplete, err := s.changeGrant(ctx, c, g, q.Reason)
		status := "approved"
		if revoke {
			status = "revoked"
		}
		if incomplete {
			status = reasonAuthorityIncomplete
		}
		return apicontract.Grant{ID: grantID(g), Repo: g.Repository, File: g.File, Step: g.Step, Path: g.Path, Status: status}, err
	}), nil
}
func grantID(g stateport.GrantChange) string {
	return string(g.Kind) + ":" + g.Repository + ":" + g.File + ":" + g.Step + ":" + g.Path + ":" + g.Tier
}
func (s *Service) PrepareAccessAllow(ctx context.Context, c apicontract.Caller, q apicontract.AccessAllowArgs) (apicontract.Call[apicontract.Grant], error) {
	return s.prepareAccess(ctx, c, q, false)
}
func (s *Service) PrepareAccessRevoke(ctx context.Context, c apicontract.Caller, q apicontract.AccessRevokeArgs) (apicontract.Call[apicontract.Receipt], error) {
	args := apicontract.AccessAllowArgs(q)
	call, e := s.prepareAccess(ctx, c, args, true)
	if e != nil {
		return nil, e
	}
	return func(ctx context.Context) (apicontract.Receipt, error) {
		g, err := call(ctx)
		v := apicontract.Receipt{ID: g.ID, State: g.Status}
		if g.Status == reasonAuthorityIncomplete {
			v.Warning = warningAuthorityIncomplete
		}
		return v, err
	}, nil
}
func (s *Service) PrepareAccessDismiss(ctx context.Context, c apicontract.Caller, q apicontract.AccessDismissArgs) (apicontract.Call[apicontract.Receipt], error) {
	a, e := s.peopleAllow(ctx, c.Actor, "access_dismiss")
	if e != nil {
		return nil, e
	}
	r, e := s.resolveRepo(ctx, a, q.Repo)
	if e != nil {
		return nil, e
	}
	g, e := grantInput(apicontract.AccessAllowArgs{Repo: r.Canonical, Secret: q.Secret, Workflow: q.Workflow, Step: q.Step})
	if e != nil {
		return nil, e
	}
	g.Dismiss = true
	c.Actor = a
	return bounded(func(ctx context.Context) (apicontract.Receipt, error) {
		incomplete, err := s.changeGrant(ctx, c, g, q.Reason)
		if err != nil {
			return apicontract.Receipt{}, err
		}
		v := apicontract.Receipt{ID: grantID(g), State: "dismissed"}
		if incomplete {
			v.Warning = warningAuthorityIncomplete
		}
		return v, nil
	}), nil
}
func (s *Service) grantPage(ctx context.Context, c apicontract.Caller, q apicontract.AccessListArgs, action string) (apicontract.Call[apicontract.GrantPage], error) {
	a, e := s.peopleAllow(ctx, c.Actor, action)
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
	return bounded(func(ctx context.Context) (apicontract.GrantPage, error) {
		v, err := s.c.People.Grants.Grants(ctx, scope)
		out := apicontract.GrantPage{Grants: []apicontract.Grant{}}
		for _, g := range v.Grants {
			if !grantMatches(g, q) {
				continue
			}
			status := "approved"
			if g.Revoke {
				status = "revoked"
			}
			out.Grants = append(out.Grants, apicontract.Grant{ID: grantID(g), Repo: g.Repository, File: g.File, Step: g.Step, Path: g.Path, Status: status})
		}
		return out, err
	}), nil
}
func (s *Service) PrepareAccessList(ctx context.Context, c apicontract.Caller, q apicontract.AccessListArgs) (apicontract.Call[apicontract.GrantPage], error) {
	return s.grantPage(ctx, c, q, "access_list")
}
func (s *Service) PrepareAdminAccessList(ctx context.Context, c apicontract.Caller, _ apicontract.AdminAccessListArgs) (apicontract.Call[apicontract.GrantPage], error) {
	return s.grantPage(ctx, c, apicontract.AccessListArgs{Kind: string(stateport.GrantStorePath)}, "admin_access_list")
}
func (s *Service) capability(ctx context.Context, q apicontract.WorkflowCapabilityGrantArgsCapability) (apicontract.Capability, error) {
	v := apicontract.Capability{Namespace: q.Namespace, ServiceAccount: q.ServiceAccount, ResourceKinds: []apicontract.ResourceKind{}}
	for _, kind := range q.ResourceKinds {
		v.ResourceKinds = append(v.ResourceKinds, apicontract.ResourceKind{APIVersion: kind.ApiVersion, Kind: kind.Kind, Actions: append([]string{}, kind.Actions...)})
	}
	if err := s.c.People.Cluster.ValidateCapability(ctx, v); err != nil {
		return v, err
	}
	if s.c.People.Cluster.ServerNamespace(v.Namespace) {
		return v, bad("namespace")
	}
	for _, kind := range v.ResourceKinds {
		ok, err := s.c.People.Cluster.Namespaced(ctx, kind.APIVersion, kind.Kind)
		if err != nil {
			return v, err
		}
		if !ok {
			return v, bad("resource_kinds")
		}
	}
	return v, nil
}

// exactAuthorityPath rejects command or traversal syntax in names interpreted by
// the installation ports, rather than relying on a downstream shell to quote it.
func exactAuthorityPath(v string) bool {
	return v != "" && path.Clean(v) == v && !strings.ContainsAny(v, "\x00\r\n") && !strings.HasPrefix(v, "/")
}
func capabilityJSON(v apicontract.Capability) json.RawMessage { raw, _ := json.Marshal(v); return raw }

func grantMatches(g stateport.GrantChange, q apicontract.AccessListArgs) bool {
	if q.Kind != "" && q.Kind != string(g.Kind) {
		return false
	}
	if q.Claim != "" && q.Claim != g.Path {
		return false
	}
	if q.Tier != "" && q.Tier != g.Tier {
		return false
	}
	return !g.Dismiss && (!g.Revoke || q.Revoked)
}

// grantTuple checks the values of a workflow-step grant; each refusal names
// the access argument that carries the value.
func grantTuple(q apicontract.AccessAllowArgs, g stateport.GrantChange) error {
	key := workflowpolicy.GrantKey{Key: identityname.Key{Repository: g.Repository, File: g.File, Step: g.Step}, Path: g.Path}
	if g.Kind == stateport.GrantAudience {
		if q.Secret != "identity" {
			return bad("secret")
		}
		key.Audience = q.Audience
	}
	if err := key.Validate(); err != nil {
		var invalid *workflowpolicy.InvalidError
		if errors.As(err, &invalid) {
			return bad(map[string]string{"repo": "repo", "file": "workflow", "step": "step", "path": "secret", "audience": "audience"}[invalid.Path])
		}
		return err
	}
	return nil
}
