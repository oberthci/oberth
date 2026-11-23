package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/stateport/contracttest"
)

func TestPublicationChronologicalPages(t *testing.T) {
	options := testOptions()
	now := options.Clock()
	options.Clock = func() time.Time { return now }
	s, err := Create(t.Context(), filepath.Join(t.TempDir(), "state.sqlite"), options)
	requireOK(t, err)
	t.Cleanup(func() { requireOK(t, s.Close()) })
	repo := atomicRepository(t, s)
	contracttest.PublicationOrder(t, s, repo.Canonical, func(at time.Time) { now = at })
}
