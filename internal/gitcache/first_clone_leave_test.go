package gitcache

import (
	"context"
	"errors"
	"testing"
)

// A caller whose deadline falls together with the transfer's end either
// marks a running transfer (which then counts it as gone) or, when the
// transfer already ended, takes its result: it is never told "continues" for
// a transfer that nobody announces.
func TestLeavingCallerAndEndingTransferNeverLoseTheOutcome(t *testing.T) {
	c := &Cache{transfers: &transfers{running: map[string]*transfer{}}}
	running := &transfer{done: make(chan struct{}), waiting: 1}
	c.transfers.running["p"] = running
	if !c.leave("p", running) || !running.left || running.waiting != 0 {
		t.Fatal("a caller leaving a running transfer did not mark it")
	}
	ended := &transfer{done: make(chan struct{}), result: outcome{err: errors.New("clone failed")}}
	close(ended.done)
	if c.leave("q", ended) || ended.left {
		t.Fatal("a caller leaving an ended transfer marked it instead of taking its result")
	}
	expired, cancel := context.WithCancel(t.Context())
	cancel()
	// Both the deadline and the transfer's end are ready; either choice of
	// the select must return the transfer's own result.
	for range 64 {
		if err := c.waitClone(expired, ended, "q"); err == nil || err.Error() != "clone failed" {
			t.Fatal("the ended transfer's result was not returned", err)
		}
	}
}

// The refs a fetch moved go to the first caller that takes the result only,
// so two callers sharing one fetch do not announce the same moves twice.
func TestSharedFetchHandsItsMovesToOneCaller(t *testing.T) {
	c := &Cache{transfers: &transfers{running: map[string]*transfer{}}}
	ended := &transfer{done: make(chan struct{}), waiting: 2, result: outcome{updates: []RefUpdate{{Ref: "refs/heads/main", NewSHA: "a"}}}}
	close(ended.done)
	first, ok := c.awaitTransfer(t.Context(), "p", ended)
	second, again := c.awaitTransfer(t.Context(), "p", ended)
	if !ok || !again || len(first.updates) != 1 || len(second.updates) != 0 {
		t.Fatal("the moves did not go to exactly one caller", first, second)
	}
}
