package store

import (
	"testing"

	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

func TestCatalogListsUseRepositoryScopeAndStableCursors(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	repo := atomicRepository(t, s)
	a := stateport.Action{Actor: "operator"}
	upstream, err := s.Upstream(t.Context(), repo.Upstream)
	requireOK(t, err)
	if upstream.Kind != "git" || upstream.BaseURL != "https://oberth.ci" || upstream.KeyName != "source" {
		t.Fatal("connection facts lost", upstream)
	}
	upstreams, err := s.Upstreams(t.Context(), Page{Limit: 1})
	requireOK(t, err)
	if len(upstreams) != 1 {
		t.Fatal(upstreams)
	}
	upstreams, err = s.Upstreams(t.Context(), Page{After: upstreams[0].Name, Limit: 1})
	requireOK(t, err)
	if len(upstreams) != 0 {
		t.Fatal("upstream cursor repeated")
	}
	atomicKey(t, s)
	keys, err := s.Keys(t.Context(), KeyFilter{Page: Page{Limit: 1}})
	requireOK(t, err)
	if len(keys) != 1 {
		t.Fatal(keys)
	}
	requireOK(t, s.ArchiveRepository(t.Context(), a, repo.ID))
	repos, err := s.Repositories(t.Context(), stateport.CatalogFilter{Limit: 200})
	requireOK(t, err)
	if len(repos) != 0 {
		t.Fatal("archived repository listed active")
	}
	requireOK(t, s.UnarchiveRepository(t.Context(), a, repo.ID))
	checkCatalogScopes(t, s, repo)
}

func checkCatalogScopes(t *testing.T, s *Store, repo stateport.Repository) {
	t.Helper()
	var repos []stateport.Repository
	var err error
	for _, scope := range [][]string{nil, {repo.Canonical}, {"origin"}, {}} {
		repos, err = s.Repositories(t.Context(), stateport.CatalogFilter{Repositories: scope, Limit: 200})
		requireOK(t, err)
		expected := 0
		if scope == nil || len(scope) > 0 && scope[0] == repo.Canonical {
			expected = 1
		}
		if len(repos) != expected {
			t.Fatal("query scope must contain canonical repository names", scope, repos)
		}
	}
}

func TestAuthorityListsAndPermanentClaimOwner(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	repo := atomicRepository(t, s)
	a := stateport.Action{Actor: "operator"}
	key := workflowpolicy.GrantKey{Key: identityname.Key{Repository: repo.Canonical, File: "build.yaml", Step: "build"}, Path: "vault/path"}
	requireOK(t, s.SecretGrant(t.Context(), a, key, false))
	requireOK(t, s.Capability(t.Context(), a, CapabilityGrant{Repository: repo.Canonical, JSON: []byte(`{"enabled":true}`)}))
	filter := stateport.CatalogFilter{Repositories: []string{repo.Canonical}, Limit: 200}
	grants, err := s.SecretGrants(t.Context(), filter)
	requireOK(t, err)
	if len(grants) != 1 || grants[0].GrantKey != key {
		t.Fatal(grants)
	}
	capabilities, err := s.Capabilities(t.Context(), filter)
	requireOK(t, err)
	if len(capabilities) != 1 {
		t.Fatal(capabilities)
	}
	requireOK(t, s.RemoveRepository(t.Context(), a, repo.ID))
	next, err := s.RegisterRepository(t.Context(), a, repo.Canonical)
	requireOK(t, err)
	if next.ID == repo.ID {
		t.Fatal("repository durable ID reused")
	}
	grants, err = s.SecretGrants(t.Context(), filter)
	requireOK(t, err)
	if len(grants) != 0 {
		t.Fatal("repository re-add revived a secret grant")
	}
	_, err = s.ReadCapability(t.Context(), repo.Canonical)
	requireCode(t, err, publicerr.NotFound)
	owner := ClaimOwner{Claim: "cache", Repository: repo.Canonical, Tier: "ci"}
	requireOK(t, s.GrantClaim(t.Context(), a, owner))
	got, err := s.ClaimOwner(t.Context(), owner.Claim)
	requireOK(t, err)
	if got != owner {
		t.Fatal(got)
	}
	requireCode(t, s.RemoveRepository(t.Context(), a, next.ID), publicerr.Conflict)
}

func TestRunLookupFiltersBeforeResolutionAndPagination(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, input := accepted(t, s, "prefix-first")
	input = reacceptance(t, input, "prefix-second")
	_, err := acceptForTest(t.Context(), s, input)
	requireOK(t, err)
	for _, query := range []func() (stateport.Run, error){
		func() (stateport.Run, error) { return s.RunByCommit(t.Context(), []string{r.Repo}, *r.CommitSHA) },
		func() (stateport.Run, error) { return s.RunByRef(t.Context(), []string{r.Repo}, r.Ref) },
	} {
		got, err := query()
		requireOK(t, err)
		if got.ID != "prefix-second" {
			t.Fatal("lookup not newest", got.ID)
		}
	}
	_, err = s.ResolveRun(t.Context(), nil, "prefix-")
	requireCode(t, err, publicerr.Ambiguous)
	got, err := s.ResolveRun(t.Context(), []string{r.Repo}, r.ID)
	requireOK(t, err)
	if got.Admission.Digest != r.Admission.Digest {
		t.Fatal("prefix detail lost admission")
	}
	_, err = s.ResolveRun(t.Context(), []string{}, r.ID)
	requireCode(t, err, publicerr.NotFound)
	pubs, err := s.Publications(t.Context(), stateport.RecordFilter{Repositories: []string{}, Limit: 200})
	requireOK(t, err)
	if len(pubs) != 0 {
		t.Fatal(pubs)
	}
}
