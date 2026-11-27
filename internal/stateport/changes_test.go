package stateport

import "testing"

func TestScopedSubscriptionsSurviveReadAndCleanUp(t *testing.T) {
	t.Parallel()
	var hub ChangeHub
	sub := hub.Subscribe(ChangeScope{Repositories: []string{"origin/team/repo"}, Runs: []string{"run"}})
	hub.Publish([]Change{{Repository: "origin/team/repo", Run: "run"}, {Run: "run"}})
	<-sub.C
	select {
	case <-sub.C:
		t.Fatal("keys duplicated the pending wake")
	default:
	}
	// A second commit during a read leaves a fresh hint for the next read.
	hub.Publish([]Change{{Run: "run"}})
	<-sub.C
	sub.Close()
	sub.Close()
	if len(hub.keys) != 0 {
		t.Fatal("empty registry keys survived unsubscribe", len(hub.keys))
	}
}
