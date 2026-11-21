// Package storetest constructs fixtures through the durable store's public operations.
// Spec: docs/plans/c7-durable-state.md#owned-data-and-public-contract
package storetest

import (
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
)

func Repository(t *testing.T, state *store.Store) stateport.Repository {
	t.Helper()
	if err := state.RegisterUpstream(t.Context(), stateport.Action{Actor: "fixture"}, store.Upstream{Name: "origin", Org: "team", Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source"}); err != nil {
		t.Fatal(err)
	}
	repository, err := state.RegisterRepository(t.Context(), stateport.Action{Actor: "fixture"}, "origin/team/repo")
	if err != nil {
		t.Fatal(err)
	}
	return repository
}
