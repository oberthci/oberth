package app

import (
	"context"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"testing"
)

func TestAuthorityAdapterPreservesTierAndExactStepGrants(t *testing.T) {
	t.Parallel()
	q := admissioncontract.PrepareRequest{IdentityRequest: admissioncontract.IdentityRequest{PipelineAccount: "pipeline"}}
	p := argoworkflow.Policy{PVCClaims: map[string]bool{}}
	g := stateport.GrantChange{Kind: stateport.GrantStorePath, Repository: "origin/team/widget", File: ".oberth/build.yaml", Step: "build", Path: "ci/token"}
	for range 2 {
		if err := (authoritySnapshot{request: &q, policy: &p, revision: "42"}).apply(t.Context(), g); err != nil {
			t.Fatal(err)
		}
	}
	if len(q.Grants) != 2 || q.Grants[0].Step != "build" || q.Grants[0].Path != "ci/token" || q.Grants[0].Revision != "42" {
		t.Fatal("exact authority or identity changed", q)
	}
	assertDerivedAccounts(t, q)
	g.Kind, g.Path, g.Tier = stateport.GrantPVC, "cache", "ci"
	if err := (authoritySnapshot{request: &q, policy: &p, revision: "42"}).apply(t.Context(), g); err != nil {
		t.Fatal(err)
	}
	g.Tier = "release"
	if err := (authoritySnapshot{request: &q, policy: &p, revision: "42"}).apply(t.Context(), g); err != nil || !p.PVCClaims["cache"] {
		t.Fatal("another tier erased the current claim grant", err)
	}

}

func TestCapabilityJSONFromServiceReachesAdmission(t *testing.T) {
	t.Parallel()
	a := AuthorityReader{config: AuthorityConfig{ServerNamespace: "oberth", ArgoNamespace: "argo", Namespace: "ci"}}
	q := apicontract.Capability{Namespace: "runs", ServiceAccount: "resource", ResourceKinds: []apicontract.ResourceKind{{APIVersion: "v1", Kind: "ConfigMap", Actions: []string{"create"}}}}
	if err := a.ValidateCapability(t.Context(), q); err != nil {
		t.Fatal("canonical service capability was not decoded", err)
	}
	for _, namespace := range []string{"oberth", "argo", "ci"} {
		q.Namespace = namespace
		if err := a.ValidateCapability(t.Context(), q); err == nil {
			t.Fatal("service capability widened into the protected namespace", namespace)
		}
	}
}

func assertDerivedAccounts(t *testing.T, q admissioncontract.PrepareRequest) {
	t.Helper()
	var accounts []string
	authority := AuthorityReader{config: AuthorityConfig{EnsureAccount: func(_ context.Context, name string) error { accounts = append(accounts, name); return nil }}}
	if err := authority.prepareAccounts(t.Context(), q, false); err != nil || len(accounts) != 2 {
		t.Fatal("derived step accounts were not declared once", accounts, err)
	}

}

func TestAudienceGrantKeepsURLSeparateFromCredentialPath(t *testing.T) {
	q := admissioncontract.PrepareRequest{}
	p := argoworkflow.Policy{}
	grant := stateport.GrantChange{Kind: stateport.GrantAudience, Repository: "origin/team/widget", File: ".oberth/build.yaml", Step: "build", Path: "https://identity.example.test"}
	if err := (authoritySnapshot{request: &q, policy: &p, revision: "1"}).apply(t.Context(), grant); err != nil {
		t.Fatal(err)
	}
	if len(q.Grants) != 1 || q.Grants[0].Path != "identity" || q.Grants[0].Audience != grant.Path {
		t.Fatal("audience was interpreted as a credential path", q.Grants)
	}
}
