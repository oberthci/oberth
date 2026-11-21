package gitcache

import (
	"context"
	"errors"
	"testing"
)

// A caller whose deadline falls together with the clone's end either marks a
// running clone (which then announces) or, when the clone already ended,
// takes its result: it is never told "continues" for a clone nobody announces.
func TestLeavingCallerAndEndingCloneNeverLoseTheOutcome(t *testing.T) {
	c := &Cache{clones: &firstClones{running: map[string]*firstClone{}}}
	running := &firstClone{done: make(chan struct{})}
	c.clones.running["p"] = running
	if !c.leave(running, "p") || !running.left {
		t.Fatal("a caller leaving a running clone did not mark it")
	}
	ended := &firstClone{done: make(chan struct{}), err: errors.New("clone failed")}
	close(ended.done)
	if c.leave(ended, "q") || ended.left {
		t.Fatal("a caller leaving an ended clone marked it instead of taking its result")
	}
	expired, cancel := context.WithCancel(t.Context())
	cancel()
	// Both the deadline and the clone's end are ready; either choice of the
	// select must return the clone's own result.
	for range 64 {
		if err := c.waitClone(expired, ended, "q"); err == nil || err.Error() != "clone failed" {
			t.Fatal("the ended clone's result was not returned", err)
		}
	}
}
