package store

import (
	"github.com/oberthci/oberth/internal/stateport/contracttest"
	"testing"
)

func TestAdministrationContract(t *testing.T) {
	t.Parallel()
	contracttest.AdministrationSuite(t, func(t *testing.T) contracttest.AdministrationFixture {
		f := serviceFixture(t)
		s := f.Port.(*Store)
		return contracttest.AdministrationFixture{Port: s, Repository: f.Repository, Now: s.options.Clock}
	})
}
