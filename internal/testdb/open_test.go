package testdb

import (
	"github.com/oberthci/oberth/internal/store"
	"testing"
	"time"
)

func TestOpenBuildsCurrentIsolatedDatabase(t *testing.T) {
	state := Open(t, store.DefaultOptions(func() time.Time { return time.Time{} }))
	if err := state.Readiness(t.Context()); err != nil {
		t.Fatal(err)
	}
}
