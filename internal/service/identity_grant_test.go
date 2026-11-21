package service_test

import (
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"testing"
)

func TestIdentityAudienceIsExactAndRevocable(t *testing.T) {
	p := people(t)
	q := apicontract.AccessAllowArgs{Repo: repository, Kind: "identity", Audience: "https://identity.example.test", Workflow: ".oberth/release.yaml", Step: "publish", Secret: "identity"}
	call, err := p.f.s.PrepareAccessAllow(t.Context(), adminCaller(), q)
	if _, err = execute(t.Context(), call, err); err != nil {
		t.Fatal(err)
	}
	snapshot, err := p.state.Grants(t.Context(), nil)
	if err != nil || len(snapshot.Grants) != 1 || snapshot.Grants[0].Kind != stateport.GrantAudience || snapshot.Grants[0].Path != q.Audience {
		t.Fatal("audience lost", snapshot, err)
	}
	revoke, err := p.f.s.PrepareAccessRevoke(t.Context(), adminCaller(), apicontract.AccessRevokeArgs{Repo: q.Repo, Kind: q.Kind, Audience: q.Audience, Workflow: q.Workflow, Step: q.Step, Secret: q.Secret})
	if _, err = execute(t.Context(), revoke, err); err != nil {
		t.Fatal(err)
	}
	snapshot, err = p.state.Grants(t.Context(), nil)
	if err != nil || len(snapshot.Grants) != 1 || !snapshot.Grants[0].Revoke {
		t.Fatal("revoked another tuple", snapshot, err)
	}
}

func TestIdentityGrantNamesTheInvalidSecretArgument(t *testing.T) {
	p := people(t)
	q := apicontract.AccessAllowArgs{Repo: repository, Kind: "identity", Audience: "publisher", Workflow: ".oberth/release.yaml", Step: "publish", Secret: "publisher"}
	_, err := p.f.s.PrepareAccessAllow(t.Context(), adminCaller(), q)
	if err == nil || publicerr.Classify(err).Reason() != "Invalid field: secret." {
		t.Fatal("wrong fourth argument blamed the audience", err)
	}
	_, err = p.f.s.PrepareAccessRevoke(t.Context(), adminCaller(), apicontract.AccessRevokeArgs(q))
	if err == nil || publicerr.Classify(err).Reason() != "Invalid field: secret." {
		t.Fatal("revoke blamed the audience", err)
	}
}

func TestIdentityGrantRejectsMissingOrMisplacedAudience(t *testing.T) {
	p := people(t)
	for _, q := range []apicontract.AccessAllowArgs{
		{Kind: "identity", Secret: "identity"},
		{Kind: "identity", Secret: "publisher", Audience: "publisher"},
		{Kind: "secret", Secret: "path", Audience: "publisher"},
		{Kind: "identity_audience", Secret: "publisher"},
	} {
		q.Repo, q.Workflow, q.Step = repository, ".oberth/build.yaml", "build"
		if _, err := p.f.s.PrepareAccessAllow(t.Context(), adminCaller(), q); err == nil {
			t.Fatal("invalid audience accepted", q)
		}
	}
}
