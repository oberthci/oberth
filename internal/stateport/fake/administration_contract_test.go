package fake_test

import (
	"github.com/oberthci/oberth/internal/stateport/contracttest"
	"github.com/oberthci/oberth/internal/stateport/fake"
	"testing"
	"time"
)

func TestAdministrationContract(t *testing.T) {
	contracttest.AdministrationSuite(t, func(t *testing.T) contracttest.AdministrationFixture {
		f := runFixture(t)
		s := f.Port.(*fake.Runs)
		return contracttest.AdministrationFixture{Port: s, Repository: f.Repository, Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}
	})
}
