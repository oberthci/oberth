package main

import (
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

func TestPreApprovedRepositoryHasIdentitiesOnRegistration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	db, err := store.CreateGenesis(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	storetest.Track(db, path, store.Options{})
	upstream, err := storetest.CreateUpstream(db, t.Context(), model.UpstreamSpec{Name: "upstream", Kind: "ssh", BaseURL: "ssh://git@upstream.example.test/org"})
	if err != nil {
		t.Fatal(err)
	}
	const repo, file, step, secret = "upstream/org/repo", ".oberth/build.yaml", "build", "oberth/data/upstream/org/repo/token"
	if _, err := db.Grant(t.Context(), repo, file, step, secret, "admin"); err != nil {
		t.Fatal(err)
	}
	identities := argojob.NewIdentityStoreWithSteps(nil, nil, nil)
	refresh := newGrantIdentityRefresher(db, identities)
	if err := refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot := identities.Snapshot()
	registered, err := db.RegisterRepository(t.Context(), "admin", model.RepositorySpec{Name: "repo", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	grants, err := db.ActiveSecretGrants(t.Context(), registered.ID, file)
	if err != nil || !grants[step][secret] {
		t.Fatal("registered repository has no active pre-approval", grants, err)
	}
	// Registration requires neither another grant mutation nor a restart.
	if identities.Snapshot() != snapshot {
		t.Fatal("registration unexpectedly replaced the identity snapshot")
	}
	identity := snapshot.Step[repo+"/"+file+"/"+step]
	if identity.ServiceAccountName != installer.PerStepName("upstream", "org", "repo", file, step) || identity.CIServiceAccountName != installer.PerStepCIName("upstream", "org", "repo", file, step) || snapshot.Release[repo].ServiceAccountName == "" || snapshot.CI[repo].ServiceAccountName == "" {
		t.Fatalf("registration lacks deterministic pre-approved identities: %+v", snapshot)
	}
}
