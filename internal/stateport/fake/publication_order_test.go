package fake_test

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stateport/contracttest"
	"github.com/oberthci/oberth/internal/stateport/fake"
)

func TestPublicationChronologicalPages(t *testing.T) {
	var now time.Time
	f := fake.NewRuns(func() time.Time { return now })
	repo := stateport.Repository{Upstream: "origin", Name: "team/repo", Canonical: "origin/team/repo"}
	f.SeedRepository(repo)
	contracttest.PublicationOrder(t, f, repo.Canonical, func(at time.Time) { now = at })
}
