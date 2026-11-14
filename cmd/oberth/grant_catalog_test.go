package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

func newGrantCatalogDatabase(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	db, err := store.CreateGenesis(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return storetest.Track(db, path, store.Options{})
}

func TestReviewBareGrantRowBreaksRefresh(t *testing.T) {
	for _, mode := range []string{"existing", "restore"} {
		t.Run(mode, func(t *testing.T) {
			db := newGrantCatalogDatabase(t)
			const file, step, secret = ".oberth/build.yaml", "build", "oberth/data/token"
			var keys []model.SecretAccessKey
			for _, repo := range []string{"typo", "org/typo", "upstream/org/typo/extra", "upstream/org/good"} {
				keys = append(keys, model.SecretAccessKey{Repo: repo, Workflow: file, Step: step, Path: secret})
			}
			if mode == "restore" {
				if err := db.ReplaceSecretAccess(t.Context(), "admin", keys); err != nil {
					t.Fatal(err)
				}
			} else {
				for _, key := range keys {
					if _, err := db.Grant(t.Context(), key.Repo, file, step, secret, "admin"); err != nil {
						t.Fatal(err)
					}
				}
			}
			rows, err := db.SecretAccessList(t.Context(), "", false)
			if err != nil || len(rows) != len(keys) {
				t.Fatalf("persisted grants: %+v, %v", rows, err)
			}
			var logs bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(previous)
			// These are the two startup producers, tested independently so a
			// failure in one cannot hide a failure in the other.
			release, repoErr := buildPerRepoIdentities(t.Context(), db)
			steps, stepErr := buildPerStepIdentities(t.Context(), db)
			if repoErr != nil || stepErr != nil {
				t.Fatalf("inert rows blocked startup: per-repo=%v, per-step=%v", repoErr, stepErr)
			}
			if len(release) != 1 || len(steps) != 1 || release["upstream/org/good"].ServiceAccountName != installer.PerRepoName("upstream", "org", "good") || steps["upstream/org/good/"+file+"/"+step].CIServiceAccountName != installer.PerStepCIName("upstream", "org", "good", file, step) {
				t.Fatalf("startup lost qualified identities or activated an inert row: release=%+v, steps=%+v", release, steps)
			}
			for _, row := range rows {
				if row.Repo == "upstream/org/good" {
					continue
				}
				for _, producer := range []string{"per-repo", "per-step"} {
					message := fmt.Sprintf("%s identities: skipping secret grant row %d for repository %q; requires upstream/org/repo", producer, row.ID, row.Repo)
					if strings.Count(logs.String(), message) != 1 {
						t.Errorf("missing single row diagnostic %q: %s", message, logs.String())
					}
				}
			}
			identities := argojob.NewIdentityStoreWithSteps(nil, nil, nil)
			if err := newGrantIdentityRefresher(db, identities)(t.Context()); err != nil {
				t.Fatalf("inert rows blocked refresh: %v", err)
			}
			if snapshot := identities.Snapshot(); len(snapshot.Release) != 1 || len(snapshot.CI) != 1 || len(snapshot.Step) != 1 {
				t.Fatalf("refresh published inert identities: %+v", snapshot)
			}
			upstream, err := storetest.CreateUpstream(db, t.Context(), model.UpstreamSpec{Name: "upstream", Kind: "ssh", BaseURL: "ssh://git@upstream.example.test/org"})
			if err != nil {
				t.Fatal(err)
			}
			repo, err := db.RegisterRepository(t.Context(), "admin", model.RepositorySpec{Name: "typo", UpstreamID: upstream.ID, DefaultBranch: "main"})
			if err != nil {
				t.Fatal(err)
			}
			if grants, err := db.ActiveSecretGrants(t.Context(), repo.ID, file); err != nil || len(grants) != 0 {
				t.Fatalf("registration activated unqualified grants: %+v, %v", grants, err)
			}
		})
	}
}

func TestReviewAccessAllowBareRepoBricksRefresh(t *testing.T) {
	db := newGrantCatalogDatabase(t)
	identities := argojob.NewIdentityStoreWithSteps(nil, nil, nil)
	refresh := newGrantIdentityRefresher(db, identities)
	s, err := service.NewAPI(service.APIConfig{Repositories: db, SecretAccess: db, Runs: db, Auditor: db, SecretAccessChanged: refresh})
	if err != nil {
		t.Fatal(err)
	}
	owner := api.Actor{Identity: "owner", Admin: true}
	call := func(name, repo string) error {
		t.Helper()
		args, err := json.Marshal(model.SecretAccessKey{Repo: repo, Workflow: ".oberth/build.yaml", Step: "build", Path: "oberth/data/token"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.CallTool(t.Context(), owner, name, args)
		return err
	}
	for _, spelling := range []string{"typo", "org/typo"} {
		if err := call("access_allow", spelling); !errors.Is(err, service.ErrInvalidInput) || !strings.Contains(err.Error(), "upstream/org/repo") {
			t.Fatalf("unregistered spelling %q was not refused before commit: %v", spelling, err)
		}
	}
	if rows, err := db.SecretAccessList(t.Context(), "", true); err != nil || len(rows) != 0 {
		t.Fatalf("refused spellings committed grant rows: %+v, %v", rows, err)
	}
	// A valid grant must still refresh with an old bare row in the database.
	if _, err := db.Grant(t.Context(), "typo", ".oberth/build.yaml", "build", "oberth/data/existing", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := call("access_allow", "upstream/org/good"); err != nil {
		t.Fatalf("later valid grant could not refresh: %v", err)
	}
	if snapshot := identities.Snapshot(); len(snapshot.Release) != 1 || len(snapshot.CI) != 1 || len(snapshot.Step) != 1 || snapshot.Step["upstream/org/good/.oberth/build.yaml/build"].CIServiceAccountName == "" {
		t.Fatalf("later qualified grant missing from identity snapshot: %+v", snapshot)
	}
	if err := refresh(t.Context()); err != nil {
		t.Fatalf("persisted grants would block restart: %v", err)
	}
	if err := call("access_revoke", "upstream/org/good"); err != nil {
		t.Fatalf("later valid revoke could not refresh: %v", err)
	}
	if snapshot := identities.Snapshot(); len(snapshot.Release) != 0 || len(snapshot.CI) != 0 || len(snapshot.Step) != 0 {
		t.Fatalf("revoked or inert grants retained identities: %+v", snapshot)
	}
}
