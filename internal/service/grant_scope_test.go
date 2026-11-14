package service

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

func TestScopedAdminCannotMutateSecretGrants(t *testing.T) {
	db, err := openTestStore(t, t.Context(), filepath.Join(t.TempDir(), "oberth.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	upstream, err := storetest.CreateUpstream(db, t.Context(), model.UpstreamSpec{Name: "upstream", Kind: "ssh", BaseURL: "ssh://git@upstream.example.test/org"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateRepository(t.Context(), model.RepositorySpec{Name: "repo", UpstreamID: upstream.ID, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	s, err := NewAPI(APIConfig{Repositories: db, SecretAccess: db, Runs: db, Auditor: db})
	if err != nil {
		t.Fatal(err)
	}
	key := model.SecretAccessKey{Repo: "upstream/org/repo", Workflow: ".oberth/release.yaml", Step: "publish", Path: "oberth/data/release/cosign-secret"}
	args, _ := json.Marshal(key)
	owner := api.Actor{Identity: "owner", Admin: true}
	if _, err := s.CallTool(t.Context(), owner, "access_allow", args); err != nil {
		t.Fatal("unscoped admin grant", err)
	}
	before, err := db.SecretAccessList(t.Context(), "", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"access_allow", "access_revoke"} {
		t.Run(action, func(t *testing.T) {
			actor := api.Actor{Identity: "scoped-admin", Admin: true, Scope: []string{"upstream"}}
			if _, err := s.CallTool(t.Context(), actor, action, args); !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "unscoped admin") {
				t.Fatalf("scoped admin could mutate an in-scope release grant: %v", err)
			}
			after, err := db.SecretAccessList(t.Context(), "", true)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("refused action changed grant rows: %v, %v", after, err)
			}
			value, err := s.CallTool(t.Context(), actor, "access_list", json.RawMessage(`{"repo":"upstream/org/repo"}`))
			if err != nil || len(value.(api.AccessListResponse).Grants) != 1 {
				t.Fatalf("scoped read-only list: %v, %v", value, err)
			}
		})
	}
	if _, err := s.CallTool(t.Context(), owner, "access_revoke", args); err != nil {
		t.Fatal("unscoped admin revoke", err)
	}
}
