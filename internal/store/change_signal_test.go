package store

import (
	"github.com/oberthci/oberth/internal/stateport"
	"testing"
)

func observeChanges(t *testing.T, port interface {
	Subscribe(stateport.ChangeScope) stateport.Subscription
}) <-chan struct{} {
	t.Helper()
	sub := port.Subscribe(stateport.ChangeScope{All: true})
	t.Cleanup(sub.Close)
	return sub.C
}
