package store

import (
	"github.com/oberthci/oberth/internal/stateport/contracttest"
	"testing"
)

func TestMovingStateContract(t *testing.T) { contracttest.MovingSuite(t, serviceFixture) }
