package service

import (
	"encoding/json"
	"errors"
	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
	"strings"
	"testing"
)

func TestRepositoryToolsListVisibleQualifiedCandidates(t *testing.T) {
	f := newControlFixture(t)
	control := f.api(t)
	commit := strings.Repeat("a", 40)
	repos := []model.Repository{f.repo}
	for _, name := range []string{"mirror", "hidden"} {
		up, err := storetest.CreateUpstream(f.store, t.Context(), model.UpstreamSpec{Name: name, Kind: "ssh", BaseURL: "ssh://git@example.com/acme"})
		if err != nil {
			t.Fatal(err)
		}
		repo, err := f.store.CreateRepository(t.Context(), model.RepositorySpec{Name: "oberth", UpstreamID: up.ID, DefaultBranch: "main"})
		if err != nil {
			t.Fatal(err)
		}
		repos = append(repos, repo)
	}
	for _, repo := range repos {
		_, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature/x", SHA: commit, Actor: "admin", Trigger: "branch"})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, scoped := range []bool{false, true} {
		actor := api.Actor{Identity: "admin", Admin: true}
		if scoped {
			actor.Scope = []string{"codeberg", "mirror"}
		}
		for _, tool := range []string{"promote", "sync", "status", "wait", "run_list", "promotion_list", "issue_list"} {
			for _, repo := range []string{"oberth", "acme/oberth", ""} {
				if repo == "" && (tool == "run_list" || tool == "promotion_list" || tool == "issue_list") {
					continue
				}
				args := map[string]any{"repo": repo}
				switch tool {
				case "promote":
					args["sha"] = commit
					args["branch"] = "main"
				case "sync", "wait":
					args["sha"] = commit
				case "status":
					args["ref"] = commit
				}
				raw, _ := json.Marshal(args)
				_, err := control.CallTool(t.Context(), actor, tool, raw)
				if !errors.Is(err, store.ErrAmbiguous) && !errors.Is(err, ErrAmbiguousRepository) {
					t.Fatalf("%s(%s) scoped=%v: %v", tool, repo, scoped, err)
				}
				for _, name := range []string{"codeberg/acme/oberth", "mirror/acme/oberth"} {
					if !strings.Contains(err.Error(), name) {
						t.Errorf("%s(%s) missing %s: %v", tool, repo, name, err)
					}
				}
				if scoped && strings.Contains(err.Error(), "hidden/acme/oberth") {
					t.Fatal("hidden repository disclosed")
				}
				if !scoped && !strings.Contains(err.Error(), "hidden/acme/oberth") {
					t.Errorf("%s(%s) omitted third match: %v", tool, repo, err)
				}
			}
		}
	}
}
