package service_test

import (
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/store"
)

func TestLandedStoreComposesWithServiceRecordingAndRunFacts(t *testing.T) {
	t.Parallel()
	f := setup(t)
	state := landedStore(t, f)
	repo := landedRepository(t, state)
	a := stateport.Action{Actor: admin().Identity}
	f.c.Runs, f.c.Catalog, f.c.Events = state, state, state
	f.restart(t)
	if _, err := f.s.Decide(t.Context(), update()); err != nil {
		t.Fatal(err)
	}
	b := batch(t)
	b.Runs[0].Prepared = landedAdmission(t, runID, b.Runs[0].Request)
	if err := state.ArchiveRepository(t.Context(), a, repo.ID); err != nil {
		t.Fatal(err)
	}
	f.store.Actors[caller().Fingerprint] = actor.Actor{Identity: "revoked"}
	result, err := f.s.Accept(t.Context(), b)
	if err != nil || !result.Fresh || len(result.Runs) != 1 || result.Runs[0].RepositoryID != repo.ID {
		t.Fatal("landed recording seam failed", result, err)
	}
	r := result.Runs[0]
	if _, err := state.BeginEvidence(t.Context(), stateport.Action{}, stateport.EvidenceStart{RunID: r.ID, Nodes: 1}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Finished(t.Context(), service.FinishedRun{RunID: r.ID, Status: stepmodel.RunFailed}); err != nil {
		t.Fatal("native observation defeated terminal fact", err)
	}
	stored, err := state.Run(t.Context(), r.ID)
	if err != nil || stored.Status != stepmodel.RunFailed || stored.Admission.Digest != b.Runs[0].Prepared.View().Digest {
		t.Fatal("real store lost verdict or seal", stored, err)
	}
}

func landedStore(t *testing.T, f *fixture) *store.Store {
	t.Helper()
	state, _ := landedStoreWithPath(t, f)
	return state
}

func landedStoreWithPath(t *testing.T, f *fixture) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	state, err := store.Create(t.Context(), path, store.DefaultOptions(f.c.Now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := state.Close(); err != nil {
			t.Error(err)
		}
	})
	return state, path
}

func landedRepository(t *testing.T, state *store.Store) stateport.Repository {
	t.Helper()
	a := stateport.Action{Actor: admin().Identity}
	if err := state.RegisterUpstream(t.Context(), a, stateport.Upstream{Name: "origin", Org: "team", Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source"}); err != nil {
		t.Fatal(err)
	}
	repo, err := state.RegisterRepository(t.Context(), a, repository)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}
