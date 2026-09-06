package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type fakeUpstreamCatalog struct {
	// repositories is keyed by the exact lookup selector, mirroring the
	// store's resolution forms: bare ("repo"), org-qualified ("org/repo"),
	// and fully qualified ("upstream/org/repo"). Register a repo under every
	// selector the scenario should resolve.
	repositories map[string]model.Repository
	// ambiguous marks bare names that exist under multiple upstreams; the
	// store answers those with ErrAmbiguous rather than picking one.
	ambiguous map[string]bool
	upstreams []model.Upstream
}

func (catalog fakeUpstreamCatalog) RepositoryByName(_ context.Context, name string) (model.Repository, error) {
	if catalog.ambiguous[name] {
		return model.Repository{}, fmt.Errorf("%w: repository %q exists under multiple upstreams; qualify as org/repo or upstream/org/repo", store.ErrAmbiguous, name)
	}
	value, ok := catalog.repositories[name]
	if !ok {
		return model.Repository{}, store.ErrNotFound
	}
	return value, nil
}

func (catalog fakeUpstreamCatalog) Upstream(_ context.Context, id int64) (model.Upstream, error) {
	for _, value := range catalog.upstreams {
		if value.ID == id {
			return value, nil
		}
	}
	return model.Upstream{}, store.ErrNotFound
}

func (catalog fakeUpstreamCatalog) ListUpstreams(context.Context) ([]model.Upstream, error) {
	return append([]model.Upstream(nil), catalog.upstreams...), nil
}

func TestUpstreamsUsesDurableRepositoryMapping(t *testing.T) {
	t.Parallel()
	catalog := fakeUpstreamCatalog{
		repositories: map[string]model.Repository{"oberth": {Name: "oberth", UpstreamID: 2}},
		upstreams: []model.Upstream{
			{ID: 1, Name: "other", BaseURL: "ssh://git@example.invalid/other"},
			{ID: 2, Name: "codeberg", BaseURL: "ssh://git@codeberg.org/acme"},
		},
	}
	remote, err := (Upstreams{Catalog: catalog}).Remote("oberth")
	if err != nil {
		t.Fatal(err)
	}
	if remote != "ssh://git@codeberg.org/acme/oberth.git" {
		t.Fatalf("remote = %q", remote)
	}
}

func TestUpstreamsDiscoversOnlyAgainstSoleUpstream(t *testing.T) {
	t.Parallel()
	catalog := fakeUpstreamCatalog{upstreams: []model.Upstream{{ID: 7, Name: "local", BaseURL: filepath.Join(t.TempDir(), "forge")}}}
	resolver := Upstreams{Catalog: catalog}
	remote, err := resolver.Remote("new-repo")
	if err != nil {
		t.Fatal(err)
	}
	if remote != filepath.Join(catalog.upstreams[0].BaseURL, "new-repo.git") {
		t.Fatalf("remote = %q", remote)
	}
	discovered, err := resolver.DiscoverRepository(context.Background(), "new-repo")
	if err != nil {
		t.Fatal(err)
	}
	if discovered != (model.RepositorySpec{Name: "new-repo", UpstreamID: 7}) {
		t.Fatalf("discovery = %+v", discovered)
	}
}

func TestUpstreamsRejectsAmbiguousUnknownRepository(t *testing.T) {
	t.Parallel()
	catalog := fakeUpstreamCatalog{upstreams: []model.Upstream{{ID: 1}, {ID: 2}}}
	_, err := (Upstreams{Catalog: catalog}).Remote("unknown")
	if err == nil || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ambiguous remote error = %v", err)
	}
}

func TestOrgQualifiedResolvesMatchingUpstream(t *testing.T) {
	t.Parallel()
	catalog := fakeUpstreamCatalog{
		upstreams: []model.Upstream{
			{ID: 1, Name: "github", BaseURL: "ssh://git@github.com/oberthci"},
			{ID: 2, Name: "codeberg", BaseURL: "ssh://git@codeberg.org/cloudtaser"},
		},
	}
	resolver := Upstreams{Catalog: catalog}

	// org "oberthci" matches the github upstream
	remote, err := resolver.Remote("oberthci/oberth")
	if err != nil {
		t.Fatal(err)
	}
	if remote != "ssh://git@github.com/oberthci/oberth.git" {
		t.Fatalf("remote = %q", remote)
	}

	// org "cloudtaser" matches the codeberg upstream
	remote, err = resolver.Remote("cloudtaser/operator")
	if err != nil {
		t.Fatal(err)
	}
	if remote != "ssh://git@codeberg.org/cloudtaser/operator.git" {
		t.Fatalf("remote = %q", remote)
	}
}

func TestOrgQualifiedRejectsUnknownOrg(t *testing.T) {
	t.Parallel()
	catalog := fakeUpstreamCatalog{
		upstreams: []model.Upstream{
			{ID: 1, Name: "github", BaseURL: "ssh://git@github.com/oberthci"},
			{ID: 2, Name: "codeberg", BaseURL: "ssh://git@codeberg.org/cloudtaser"},
		},
	}
	_, err := (Upstreams{Catalog: catalog}).Remote("unknown-org/repo")
	if err == nil {
		t.Fatal("expected error for unknown org")
	}
	if !strings.Contains(err.Error(), "no upstream registered for") {
		t.Fatalf("error = %q, want mention of no upstream registered", err.Error())
	}
	if !strings.Contains(err.Error(), "oberthci") || !strings.Contains(err.Error(), "cloudtaser") {
		t.Fatalf("error = %q, want available upstreams listed", err.Error())
	}
}

func TestOrgQualifiedAmbiguousWithoutOrg(t *testing.T) {
	t.Parallel()
	catalog := fakeUpstreamCatalog{
		upstreams: []model.Upstream{
			{ID: 1, Name: "github", BaseURL: "ssh://git@github.com/oberthci"},
			{ID: 2, Name: "codeberg", BaseURL: "ssh://git@codeberg.org/cloudtaser"},
		},
	}
	_, err := (Upstreams{Catalog: catalog}).Remote("new-repo")
	if err == nil {
		t.Fatal("expected error for ambiguous bare repo with multiple upstreams")
	}
	if !strings.Contains(err.Error(), "org/repo format") {
		t.Fatalf("error = %q, want suggestion to use org/repo format", err.Error())
	}
}

func TestOrgQualifiedValidatesAgainstMappedUpstream(t *testing.T) {
	t.Parallel()
	catalog := fakeUpstreamCatalog{
		repositories: map[string]model.Repository{"oberth": {Name: "oberth", UpstreamID: 1}},
		upstreams: []model.Upstream{
			{ID: 1, Name: "github", BaseURL: "ssh://git@github.com/oberthci"},
			{ID: 2, Name: "codeberg", BaseURL: "ssh://git@codeberg.org/cloudtaser"},
		},
	}
	// Correct org works
	remote, err := (Upstreams{Catalog: catalog}).Remote("oberthci/oberth")
	if err != nil {
		t.Fatalf("correct org failed: %v", err)
	}
	if remote != "ssh://git@github.com/oberthci/oberth.git" {
		t.Fatalf("remote = %q", remote)
	}
	// Rewritten for issue #264: an org-qualified path names that org's OWN
	// namespace. "cloudtaser/oberth" is no longer a "wrong org" for the
	// github-registered "oberth" — it selects the codeberg upstream (org
	// cloudtaser), where a same-named repository may legitimately live. The
	// trust property that matters is that it can NEVER resolve to the
	// github repository's remote.
	remote, err = (Upstreams{Catalog: catalog}).Remote("cloudtaser/oberth")
	if err != nil {
		t.Fatalf("org-owned namespace resolution failed: %v", err)
	}
	if remote != "ssh://git@codeberg.org/cloudtaser/oberth.git" {
		t.Fatalf("remote = %q, want the cloudtaser org's own upstream", remote)
	}
	if strings.Contains(remote, "github.com") {
		t.Fatalf("remote = %q leaked the other org's upstream", remote)
	}
}

func TestOrgQualifiedDiscoverRepository(t *testing.T) {
	t.Parallel()
	catalog := fakeUpstreamCatalog{
		upstreams: []model.Upstream{
			{ID: 1, Name: "github", BaseURL: "ssh://git@github.com/oberthci"},
			{ID: 2, Name: "codeberg", BaseURL: "ssh://git@codeberg.org/cloudtaser"},
		},
	}
	resolver := Upstreams{Catalog: catalog}
	spec, err := resolver.DiscoverRepository(context.Background(), "cloudtaser/new-repo")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "new-repo" || spec.UpstreamID != 2 {
		t.Fatalf("discovery = %+v, want {Name: new-repo, UpstreamID: 2}", spec)
	}
}

func TestValidateUpstreamBaseRejectsRepositoryAndCredentials(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		"ssh://git@codeberg.org/acme/oberth.git",
		"https://user:secret@example.invalid/acme",
		"https://example.invalid/acme?token=secret",
	} {
		if err := ValidateUpstreamBase(value); err == nil {
			t.Fatalf("base %q passed validation", value)
		}
	}
	kind, err := UpstreamKind("ssh://git@codeberg.org/acme")
	if err != nil || kind != "ssh" {
		t.Fatalf("kind = %q, %v", kind, err)
	}
}

func TestMismatchErrorsWrapErrUpstreamRefused(t *testing.T) {
	t.Parallel()
	catalog := fakeUpstreamCatalog{
		repositories: map[string]model.Repository{"oberth": {Name: "oberth", UpstreamID: 1}},
		upstreams: []model.Upstream{
			{ID: 1, Name: "github", BaseURL: "ssh://git@github.com/oberthci"},
			{ID: 2, Name: "codeberg", BaseURL: "ssh://git@codeberg.org/cloudtaser"},
		},
	}
	resolver := Upstreams{Catalog: catalog}

	for _, test := range []struct {
		name  string
		input string
	}{
		// "wrong org on mapped repo" ("cloudtaser/oberth") left this table in
		// the #264 rewrite: an org-qualified path is that org's own namespace
		// and now resolves to its upstream — the positive case is asserted in
		// TestOrgQualifiedValidatesAgainstMappedUpstream.
		{"org mismatched with named upstream", "github/cloudtaser/oberth"},
		{"wrong upstream name on mapped repo", "codeberg/oberthci/oberth"},
		{"unknown upstream name", "nonexistent/oberthci/oberth"},
		{"unknown org", "unknown-org/new-repo"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := resolver.Remote(test.input)
			if err == nil {
				t.Fatal("expected error")
			}
			if !errors.Is(err, gitcache.ErrUpstreamRefused) {
				t.Fatalf("error %q does not wrap ErrUpstreamRefused", err)
			}
		})
	}
}

func TestSameNameAcrossUpstreamsResolvesPerOrg(t *testing.T) {
	t.Parallel()
	catalog := fakeUpstreamCatalog{
		repositories: map[string]model.Repository{
			"github/oberthci/terraform":     {ID: 10, Name: "terraform", UpstreamID: 1},
			"oberthci/terraform":            {ID: 10, Name: "terraform", UpstreamID: 1},
			"codeberg/cloudtaser/terraform": {ID: 20, Name: "terraform", UpstreamID: 2},
			"cloudtaser/terraform":          {ID: 20, Name: "terraform", UpstreamID: 2},
		},
		ambiguous: map[string]bool{"terraform": true},
		upstreams: []model.Upstream{
			{ID: 1, Name: "github", BaseURL: "ssh://git@github.com/oberthci"},
			{ID: 2, Name: "codeberg", BaseURL: "ssh://git@codeberg.org/cloudtaser"},
		},
	}
	resolver := Upstreams{Catalog: catalog}

	remote, err := resolver.Remote("github/oberthci/terraform")
	if err != nil {
		t.Fatalf("github-qualified: %v", err)
	}
	if remote != "ssh://git@github.com/oberthci/terraform.git" {
		t.Fatalf("github remote = %q", remote)
	}

	remote, err = resolver.Remote("cloudtaser/terraform")
	if err != nil {
		t.Fatalf("codeberg org-qualified: %v", err)
	}
	if remote != "ssh://git@codeberg.org/cloudtaser/terraform.git" {
		t.Fatalf("codeberg remote = %q", remote)
	}

	// A bare name that exists under multiple upstreams must refuse, not
	// guess: guessing would route a push (and later its publication) into
	// the wrong trust domain.
	_, err = resolver.Remote("terraform")
	if err == nil {
		t.Fatal("bare ambiguous name must be refused")
	}
	if !errors.Is(err, store.ErrAmbiguous) || !errors.Is(err, gitcache.ErrUpstreamRefused) {
		t.Fatalf("ambiguous error = %v, want ErrAmbiguous wrapped as an upstream refusal", err)
	}
}

func TestQualifyInputReturnsCanonicalIdentity(t *testing.T) {
	t.Parallel()
	catalog := fakeUpstreamCatalog{
		repositories: map[string]model.Repository{
			"github/oberthci/terraform": {ID: 10, Name: "terraform", UpstreamID: 1},
		},
		upstreams: []model.Upstream{
			{ID: 1, Name: "github", BaseURL: "ssh://git@github.com/oberthci"},
			{ID: 2, Name: "codeberg", BaseURL: "ssh://git@codeberg.org/cloudtaser"},
		},
	}
	resolver := Upstreams{Catalog: catalog}

	qualification, err := resolver.QualifyInput("github/oberthci/terraform")
	if err != nil {
		t.Fatalf("qualify registered: %v", err)
	}
	if qualification != (gitcache.RepoQualification{UpstreamName: "github", Org: "oberthci"}) {
		t.Fatalf("qualification = %+v", qualification)
	}

	// An unregistered org-qualified input qualifies through discovery rules,
	// and the returned segments are the catalog's canonical spellings even
	// when the client cases the org differently.
	qualification, err = resolver.QualifyInput("CLOUDTASER/new-repo")
	if err != nil {
		t.Fatalf("qualify discovered: %v", err)
	}
	if qualification != (gitcache.RepoQualification{UpstreamName: "codeberg", Org: "cloudtaser"}) {
		t.Fatalf("canonical qualification = %+v", qualification)
	}
}
