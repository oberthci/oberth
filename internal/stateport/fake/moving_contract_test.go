package fake_test

import (
	"github.com/oberthci/oberth/internal/stateport/contracttest"
	"github.com/oberthci/oberth/internal/stateport/fake"
	"testing"
)

func TestMovingStateContract(t *testing.T) {
	contracttest.MovingSuite(t, func(t *testing.T) contracttest.RunFixture {
		f := runFixture(t)
		f.Port = fake.WithMoving(f.Port.(*fake.Runs))
		return f
	})
}
