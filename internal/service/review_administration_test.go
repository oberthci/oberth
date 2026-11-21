package service_test

import (
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"testing"
)

func TestTupleGrantsRejectPVCFields(t *testing.T) {
	t.Parallel()
	p := people(t)
	for _, kind := range []string{"store_path", "secret", "configmap", "image_pull_secret", "identity"} {
		for _, field := range []string{"claim", "tier"} {
			q := apicontract.AccessAllowArgs{Repo: repository, Kind: kind, Workflow: ".oberth/build.yaml", Step: "build", Secret: "path"}
			if kind == "identity" {
				q.Secret, q.Audience = "identity", "https://identity.example"
			}
			if field == "claim" {
				q.Claim = "claim"
			} else {
				q.Tier = "ci"
			}
			call, e := p.f.s.PrepareAccessAllow(t.Context(), adminCaller(), q)
			requireField(t, e, "This grant kind takes no "+field+" argument.")
			if call != nil {
				t.Fatal("PVC fields accepted for tuple", q)
			}
		}
	}
}
func TestPublicIdentityNamesAreCanonical(t *testing.T) {
	t.Parallel()
	p := people(t)
	call, e := p.f.s.PrepareAdminPublicIdentity(t.Context(), adminCaller(), apicontract.AdminPublicIdentityArgs{Name: "a//b"})
	_, e = execute(t.Context(), call, e)
	requireField(t, e, "name")

}
func TestRepoRegistrationChecksUpstreamAndFreshDefault(t *testing.T) {
	t.Parallel()
	p := people(t)
	for _, q := range []apicontract.RepoAddArgs{{Upstream: "other", Repo: repository}, {Upstream: "origin", Repo: repository, DefaultBranch: "stale"}} {
		call, e := p.f.s.PrepareRepoAdd(t.Context(), adminCaller(), q)
		_, e = execute(t.Context(), call, e)
		field := "repo/upstream"
		if q.Upstream == "origin" {
			field = "default_branch"
		}
		requireField(t, e, field)
	}
}
func TestUpstreamRegistrationRequiresSupportedScheme(t *testing.T) {
	t.Parallel()
	p := people(t)
	call, e := p.f.s.PrepareUpstreamAdd(t.Context(), adminCaller(), apicontract.UpstreamAddArgs{Name: "origin", BaseURL: "ftp://ci.example"})
	_, e = execute(t.Context(), call, e)
	requireField(t, e, "base_url")
}
func TestAuditRepositoryFilterPrecedesPaging(t *testing.T) {
	t.Parallel()
	p := people(t)
	ctx := t.Context()
	for _, repo := range []string{repository, "origin/team/other"} {
		if repo != repository {
			if _, e := p.state.RegisterRepository(ctx, stateport.Action{Actor: admin().Identity}, repo); e != nil {
				t.Fatal(e)
			}
		}
		if e := p.state.RecordAdministration(ctx, stateport.Action{Actor: admin().Identity}, model.AuditAction{Actor: admin().Identity, Action: "repo.verified", ResourceType: "repository", ResourceID: repo}); e != nil {
			t.Fatal(e)
		}
	}
	call, e := p.f.s.PrepareAuditList(ctx, adminCaller(), apicontract.AuditListArgs{Repo: repository, Action: "repo.verified", Limit: 1})
	page, e := execute(ctx, call, e)
	if e != nil || len(page.Records) != 1 || page.Records[0].ResourceID != repository || page.NextCursor == "" {
		t.Fatal("audit repo filter applied after page", page, e)
	}
}
