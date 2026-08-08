package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type fakeUpstreamCatalog struct {
	repositories map[string]model.Repository
	upstreams    []model.Upstream
}

func (catalog fakeUpstreamCatalog) RepositoryByName(_ context.Context, name string) (model.Repository, error) {
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
