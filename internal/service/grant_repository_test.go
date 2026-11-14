package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/storetest"
)

func TestAccessAllowResolvesRegisteredShortNames(t *testing.T) {
	now := time.Now()
	db := testAccessStore(t, &now)
	upstream, err := storetest.CreateUpstream(db, t.Context(), model.UpstreamSpec{Name: "upstream", Kind: "ssh", BaseURL: "ssh://git@upstream.example.test/org"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateRepository(t.Context(), model.RepositorySpec{Name: "repo", UpstreamID: upstream.ID, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	refreshes := 0
	s, err := NewAPI(APIConfig{Repositories: db, SecretAccess: db, Runs: db, Auditor: db, SecretAccessChanged: func(context.Context) error { refreshes++; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	actor := api.Actor{Identity: "admin", Admin: true}
	call := func(name, repo string) (any, error) {
		t.Helper()
		args, err := json.Marshal(model.SecretAccessKey{Repo: repo, Workflow: ".oberth/build.yaml", Step: "build", Path: "oberth/data/token"})
		if err != nil {
			t.Fatal(err)
		}
		return s.CallTool(t.Context(), actor, name, args)
	}
	for _, spelling := range []string{"repo", "org/repo", "upstream/org/repo"} {
		value, err := call("access_allow", spelling)
		if err != nil || value.(api.AccessGrantResponse).Repo != "upstream/org/repo" {
			t.Fatalf("registered spelling %q did not resolve: %+v, %v", spelling, value, err)
		}
	}
	if rows, err := db.SecretAccessList(t.Context(), "", false); err != nil || len(rows) != 1 || refreshes != 3 {
		t.Fatalf("registered spellings diverged: %+v, refreshes=%d, %v", rows, refreshes, err)
	}
	other, err := storetest.CreateUpstream(db, t.Context(), model.UpstreamSpec{Name: "other", Kind: "ssh", BaseURL: "ssh://git@other.example.test/team"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateRepository(t.Context(), model.RepositorySpec{Name: "repo", UpstreamID: other.ID, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call("access_allow", "repo"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("ambiguous short name accepted: %v", err)
	}
	// Legacy rows remain listable and revocable by their exact stored key.
	if _, err := db.Grant(t.Context(), "legacy", ".oberth/build.yaml", "build", "oberth/data/token", "admin"); err != nil {
		t.Fatal(err)
	}
	listed, err := s.CallTool(t.Context(), actor, "access_list", json.RawMessage(`{"repo":"legacy"}`))
	if err != nil || len(listed.(api.AccessListResponse).Grants) != 1 {
		t.Fatalf("legacy row no longer listable: %+v, %v", listed, err)
	}
	if _, err := call("access_revoke", "legacy"); err != nil {
		t.Fatalf("legacy row no longer revocable: %v", err)
	}
}
