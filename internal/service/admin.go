package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

type adminOperation[T any] struct {
	Name string
	Run  func(context.Context, actor.Actor) (T, error)
}

func prepareAdmin[T any](s *Service, ctx context.Context, c apicontract.Caller, op adminOperation[T]) (apicontract.Call[T], error) {
	a, e := s.peopleAllow(ctx, c.Actor, op.Name)
	if e != nil {
		return nil, e
	}
	return bounded(func(ctx context.Context) (T, error) { return op.Run(ctx, a) }), nil
}
func (s *Service) recordAdmin(ctx context.Context, a actor.Actor, q apicontract.AdminAuditRecordArgs) error {
	keys := map[string][]string{"upgrade.completed": {"from", "to", "revision"}, "rollback.completed": {"from", "to", "revision"}, "backup.completed": {"archive_sha256", "bytes", "scope", "audit_head_id", "audit_head_sha256"}, "secretstore.sync_receipt": {"revision", "paths"}}
	allowed, ok := keys[q.Action]
	if !ok {
		return bad("action")
	}
	for key := range q.Details {
		if !slices.Contains(allowed, key) {
			return bad("details")
		}
	}
	raw, e := json.Marshal(q.Details)
	if e != nil || len(raw) > 2048 {
		return bad("details")
	}
	return s.c.People.Admin.RecordAdministration(ctx, s.peopleAction(a, "administration", q.Reason, ""), model.AuditAction{Actor: a.Identity, Action: q.Action, ResourceType: "installation", Details: string(raw)})
}
func (s *Service) PrepareAdminAuditRecord(ctx context.Context, c apicontract.Caller, q apicontract.AdminAuditRecordArgs) (apicontract.Call[apicontract.Receipt], error) {
	return prepareAdmin(s, ctx, c, adminOperation[apicontract.Receipt]{"admin_audit_record", func(ctx context.Context, a actor.Actor) (apicontract.Receipt, error) {
		e := s.recordAdmin(ctx, a, q)
		return apicontract.Receipt{ID: q.Action, State: "recorded"}, e
	}})
}
func (s *Service) PrepareAdminPublicIdentity(ctx context.Context, c apicontract.Caller, q apicontract.AdminPublicIdentityArgs) (apicontract.Call[apicontract.PublicIdentity], error) {
	return prepareAdmin(s, ctx, c, adminOperation[apicontract.PublicIdentity]{"admin_public_identity", func(ctx context.Context, _ actor.Actor) (apicontract.PublicIdentity, error) {
		if !exactAuthorityPath(q.Name) {
			return apicontract.PublicIdentity{}, bad("name")
		}
		return s.c.People.Administration.PublicIdentity(ctx, q.Name)
	}})
}
func (s *Service) systemStatus(ctx context.Context, c apicontract.Caller, action string) (apicontract.Call[apicontract.SystemStatus], error) {
	return prepareAdmin(s, ctx, c, adminOperation[apicontract.SystemStatus]{action, func(ctx context.Context, a actor.Actor) (apicontract.SystemStatus, error) {
		status, err := s.c.People.Administration.Status(ctx, action)
		if err != nil {
			return apicontract.SystemStatus{}, err
		}
		if apicontract.Admin.Allows(a) {
			if s.c.People.StateMetadata == nil {
				return apicontract.SystemStatus{}, refuse(unavailable)
			}
			status.SchemaVersion, status.Protocol, err = s.c.People.StateMetadata.StateMetadata(ctx)
			if err != nil {
				return apicontract.SystemStatus{}, err
			}
		}
		return status, nil
	}})
}
func (s *Service) PrepareAdminReadiness(ctx context.Context, c apicontract.Caller, _ apicontract.AdminReadinessArgs) (apicontract.Call[apicontract.SystemStatus], error) {
	return s.systemStatus(ctx, c, "admin_readiness")
}
func (s *Service) PrepareAdminVersion(ctx context.Context, c apicontract.Caller, _ apicontract.AdminVersionArgs) (apicontract.Call[apicontract.SystemStatus], error) {
	return s.systemStatus(ctx, c, "admin_version")
}
func (s *Service) PrepareAdminIdle(ctx context.Context, c apicontract.Caller, _ apicontract.AdminIdleArgs) (apicontract.Call[apicontract.SystemStatus], error) {
	return s.systemStatus(ctx, c, "admin_idle")
}
func (s *Service) PrepareRepoAdd(ctx context.Context, c apicontract.Caller, q apicontract.RepoAddArgs) (apicontract.Call[apicontract.Repository], error) {
	return prepareAdmin(s, ctx, c, adminOperation[apicontract.Repository]{"repo_add", func(ctx context.Context, a actor.Actor) (apicontract.Repository, error) {
		if !strings.HasPrefix(q.Repo, q.Upstream+"/") {
			return apicontract.Repository{}, bad("repo/upstream")
		}
		branch, e := s.c.Source.DefaultBranch(ctx, q.Repo)
		if e != nil {
			return apicontract.Repository{}, e
		}
		if q.DefaultBranch != "" && q.DefaultBranch != strings.TrimPrefix(branch, "refs/heads/") && q.DefaultBranch != branch {
			return apicontract.Repository{}, bad("default_branch")
		}
		r, e := s.c.People.Admin.RegisterRepository(ctx, s.peopleAction(a, "repository.changed", q.Reason, q.Repo), q.Repo)
		return apicontract.Repository{Repo: r.Canonical, DefaultBranch: branch, Archived: r.Archived}, e
	}})
}
func (s *Service) repositoryAdmin(ctx context.Context, c apicontract.Caller, q repositoryChange) (apicontract.Call[apicontract.Repository], error) {
	a, e := s.peopleAllow(ctx, c.Actor, q.Action)
	if e != nil {
		return nil, e
	}
	r, e := s.resolveRepo(ctx, a, q.Repo)
	if e != nil {
		return nil, e
	}
	return bounded(func(ctx context.Context) (apicontract.Repository, error) {
		fact := s.peopleAction(a, "repository.changed", q.Reason, r.Canonical)
		var err error
		switch q.Action {
		case "repo_archive":
			err = s.c.People.Admin.ArchiveRepository(ctx, fact, r.ID)
			r.Archived = true
		case "repo_unarchive":
			err = s.c.People.Admin.UnarchiveRepository(ctx, fact, r.ID)
			r.Archived = false
		case "repo_remove":
			err = s.removeRepository(ctx, apicontract.Caller{Actor: a}, r, fact)
		case "repo_verify":
			err = s.c.People.Administration.ProbeRepository(ctx, r)
		}
		return apicontract.Repository{Repo: r.Canonical, Archived: r.Archived}, err
	}), nil
}

type repositoryChange struct{ Repo, Action, Reason string }

func (s *Service) PrepareRepoArchive(ctx context.Context, c apicontract.Caller, q apicontract.RepoArchiveArgs) (apicontract.Call[apicontract.Repository], error) {
	return s.repositoryAdmin(ctx, c, repositoryChange{q.Repo, "repo_archive", q.Reason})
}
func (s *Service) PrepareRepoUnarchive(ctx context.Context, c apicontract.Caller, q apicontract.RepoUnarchiveArgs) (apicontract.Call[apicontract.Repository], error) {
	return s.repositoryAdmin(ctx, c, repositoryChange{q.Repo, "repo_unarchive", q.Reason})
}
func (s *Service) PrepareRepoVerify(ctx context.Context, c apicontract.Caller, q apicontract.RepoVerifyArgs) (apicontract.Call[apicontract.Repository], error) {
	return s.repositoryAdmin(ctx, c, repositoryChange{Repo: q.Repo, Action: "repo_verify"})
}
func (s *Service) PrepareRepoRemove(ctx context.Context, c apicontract.Caller, q apicontract.RepoRemoveArgs) (apicontract.Call[apicontract.Receipt], error) {
	call, e := s.repositoryAdmin(ctx, c, repositoryChange{q.Repo, "repo_remove", q.Reason})
	if e != nil {
		return nil, e
	}
	return func(ctx context.Context) (apicontract.Receipt, error) {
		r, err := call(ctx)
		return apicontract.Receipt{ID: r.Repo, State: "removed"}, err
	}, nil
}
func (s *Service) PrepareAuditList(ctx context.Context, c apicontract.Caller, q apicontract.AuditListArgs) (apicontract.Call[apicontract.AuditPage], error) {
	a, e := s.peopleAllow(ctx, c.Actor, "audit_list")
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
	if q.Limit == 0 {
		q.Limit = 50
	}
	return bounded(func(ctx context.Context) (apicontract.AuditPage, error) {
		rows, err := s.c.People.Admin.AdministrativeAudit(ctx, scope, stateport.AdministrativeFilter{Action: q.Action, Actor: q.Actor, Before: q.Before, Limit: q.Limit, Since: q.Since, Until: q.Until})
		page := apicontract.AuditPage{Records: []apicontract.AuditRecord{}}
		for _, row := range rows {
			page.Records = append(page.Records, apicontract.AuditRecord{AuditAction: row.AuditAction, SHA256: row.SHA256, PreviousSHA256: row.PreviousSHA256})
		}
		if len(rows) == q.Limit {
			page.NextCursor = strconv.FormatInt(rows[len(rows)-1].ID, 10)
		}
		return page, err
	}), nil
}
func upstreamResult(v stateport.Upstream) apicontract.Upstream {
	return apicontract.Upstream{Name: v.Name, Org: v.Org, BaseURL: v.BaseURL, Pending: true, DisplayReason: "Not checked in this request."}
}
func (s *Service) PrepareUpstreamAdd(ctx context.Context, c apicontract.Caller, q apicontract.UpstreamAddArgs) (apicontract.Call[apicontract.Upstream], error) {
	return prepareAdmin(s, ctx, c, adminOperation[apicontract.Upstream]{"upstream_add", func(ctx context.Context, a actor.Actor) (apicontract.Upstream, error) {
		u, e := url.Parse(q.BaseURL)
		if e != nil || !validUpstreamURL(u) {
			return apicontract.Upstream{}, bad("base_url")
		}
		prepared, e := s.c.People.Administration.PrepareUpstream(ctx, q)
		if e != nil {
			return apicontract.Upstream{}, e
		}
		v := prepared.Binding
		e = s.c.People.Admin.RegisterUpstreamBinding(ctx, s.peopleAction(a, "upstream.changed", q.Reason, ""), v)
		if e != nil {
			return apicontract.Upstream{}, e
		}
		if e = s.persistUpstream(ctx, a, q, prepared); e != nil {
			return apicontract.Upstream{}, e
		}
		out := upstreamResult(v)
		if !q.NoWait || q.CheckRepo != "" {
			out, e = s.c.People.Administration.ProbeUpstream(ctx, v.Name, q.CheckRepo)
		}
		out.Org = v.Org
		out.Offered, out.Replaced = prepared.Offered, prepared.Replaced
		var problem *publicerr.Error
		if q.CheckRepo != "" && errors.As(e, &problem) {
			problem.Context = append(problem.Context, "Upstream is registered: "+v.Name)
		}
		return out, e
	}})
}
func (s *Service) PrepareUpstreamList(ctx context.Context, c apicontract.Caller, q apicontract.UpstreamListArgs) (apicontract.Call[apicontract.UpstreamPage], error) {
	return prepareAdmin(s, ctx, c, adminOperation[apicontract.UpstreamPage]{"upstream_list", func(ctx context.Context, _ actor.Actor) (apicontract.UpstreamPage, error) {

		rows, e := s.c.People.Admin.UpstreamBindings(ctx)
		out := apicontract.UpstreamPage{Upstreams: []apicontract.Upstream{}}
		for _, v := range rows {
			result := upstreamResult(v)
			if q.Check {
				result, e = s.c.People.Administration.ProbeUpstream(ctx, v.Name, "")
				if e != nil {
					return out, e
				}
			}
			result.Org = v.Org
			out.Upstreams = append(out.Upstreams, result)
		}
		return out, e
	}})
}
func (s *Service) PrepareUpstreamRemove(ctx context.Context, c apicontract.Caller, q apicontract.UpstreamRemoveArgs) (apicontract.Call[apicontract.Receipt], error) {
	return prepareAdmin(s, ctx, c, adminOperation[apicontract.Receipt]{"upstream_remove", func(ctx context.Context, a actor.Actor) (apicontract.Receipt, error) {
		e := s.c.People.Admin.RemoveUpstream(ctx, s.peopleAction(a, "upstream.changed", q.Reason, ""), q.Name)
		return apicontract.Receipt{ID: q.Name, State: "removed"}, e
	}})
}
func (s *Service) PrepareUpstreamProvideKey(ctx context.Context, c apicontract.Caller, q apicontract.UpstreamProvideKeyArgs) (apicontract.Call[apicontract.Upstream], error) {
	return prepareAdmin(s, ctx, c, adminOperation[apicontract.Upstream]{"upstream_provide_key", func(ctx context.Context, a actor.Actor) (apicontract.Upstream, error) {
		if !exactAuthorityPath(q.Name) {
			return apicontract.Upstream{}, bad("name")
		}
		if e := s.c.People.Admin.RecordAdministration(ctx, s.peopleAction(a, "administration", q.Reason, ""), model.AuditAction{Actor: a.Identity, Action: "upstream.provide_key", ResourceType: "upstream", ResourceID: q.Name}); e != nil {
			return apicontract.Upstream{}, e
		}
		return s.c.People.Administration.PersistKey(ctx, q)
	}})
}

// PrepareReceiveOutbox delegates recovery after C2 authorizes canonical scope.
// The recovery port resolves IDs within that scope and audits repair intent.
func (s *Service) PrepareReceiveOutbox(ctx context.Context, c apicontract.Caller, q apicontract.ReceiveOutboxArgs) (apicontract.Call[apicontract.OutboxResult], error) {
	a, e := s.peopleAllow(ctx, c.Actor, "receive_outbox")
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
		q.Repo = r.Canonical
	}
	return bounded(func(ctx context.Context) (apicontract.OutboxResult, error) {
		v, e := s.c.People.Administration.ReceiveOutbox(ctx, stateport.Action{Actor: a.Identity, Reason: q.Reason}, scope, q)
		if v.Records == nil {
			v.Records = []apicontract.OutboxRecord{}
		}
		return v, e
	}), nil
}

func validUpstreamURL(u *url.URL) bool {
	if u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Scheme != "https" && u.Scheme != "ssh" {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		return err == nil && n > 0 && n <= 65535
	}
	return !strings.HasSuffix(u.Host, ":")
}
