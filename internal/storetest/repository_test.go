package storetest

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
)

func TestRepositoryFixtureUsesAuditedRegistration(t *testing.T) {
	state, err := store.Create(t.Context(), filepath.Join(t.TempDir(), "state.sqlite"), store.Options{Protocol: stateport.DefaultProtocol(), Clock: func() time.Time { return time.Time{} }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := state.Close(); err != nil {
			t.Error(err)
		}
	}()
	fixture := Repository(t, state)
	rows, head, err := state.Audit(t.Context(), 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.Canonical != "origin/team/repo" || len(rows) != 2 || head.ID != 2 {
		t.Fatal("fixture bypassed public mutations")
	}
}
