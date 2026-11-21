package service_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestRepositoryDiscoveryIncludesOnlyLiveScopedUpstreamAddresses(t *testing.T) {
	t.Parallel()
	p := people(t)
	if err := p.state.RegisterUpstream(t.Context(), stateport.Action{Actor: "owner"}, stateport.Upstream{Name: "hidden", Org: "other", Kind: "git", BaseURL: "https://hidden.oberth.ci", KeyName: "private-key"}); err != nil {
		t.Fatal(err)
	}
	for _, scope := range [][]string{{"origin"}, {"hidden"}, {"unregistered"}} {
		a := caller()
		a.Scope = scope
		p.f.store.Actors[a.Fingerprint] = a
		call, err := p.f.s.PrepareRepoList(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.RepoListArgs{})
		if err != nil {
			t.Fatal(err)
		}
		page, err := call(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		assertDiscoveredUpstreams(t, page.Upstreams, scope[0])
		if scope[0] != "origin" && len(page.Repositories) != 0 {
			t.Fatal("repository escaped live scope", page.Repositories)
		}
	}
}

func assertDiscoveredUpstreams(t *testing.T, sources []apicontract.Upstream, scope string) {
	t.Helper()
	want := 1
	if scope == "unregistered" {
		want = 0
	}
	if len(sources) != want {
		t.Fatal("wrong live upstream scope", scope, sources)
	}
	for _, source := range sources {
		wantOrg := "team"
		if scope == "hidden" {
			wantOrg = "other"
		}
		if source.Org != wantOrg || source.Name != scope || source.BaseURL == "" || source.PublicKey != "" || source.Fingerprint != "" || len(source.Offered) != 0 {
			t.Fatal("discovery exposed wrong upstream metadata", source)
		}
	}
}

func TestUpstreamListCarriesStoredOrganization(t *testing.T) {
	t.Parallel()
	p := people(t)
	for _, check := range []bool{false, true} {
		call, err := p.f.s.PrepareUpstreamList(t.Context(), adminCaller(), apicontract.UpstreamListArgs{Check: check})
		page, err := execute(t.Context(), call, err)
		if err != nil || len(page.Upstreams) != 1 || page.Upstreams[0].Org != "team" {
			t.Fatal("stored organization missing", check, page, err)
		}
	}
}
