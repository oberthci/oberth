package gitcache_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
)

func TestFailedTransportCompletionStillNamesRestoredReference(t *testing.T) {
	f, c, next := completionFixture(t)
	f.Git(t, f.Store(), "update-ref", "refs/heads/second", f.Commit)
	r := fourthAgentPush(t, f, []gitcache.RefUpdate{{Ref: "refs/heads/topic", OldSHA: f.Commit}, {Ref: "refs/heads/second", OldSHA: f.Commit, NewSHA: next}}, nil)
	var once sync.Once
	r.Begin = func() {
		once.Do(func() { installPreparationStop(t, preparationStop{Store: f.Store(), Damage: "between"}, 0) })
	}
	status := &completionStatus{failure: io.ErrClosedPipe, failureAfter: "ok refs/heads/second"}
	var feedback bytes.Buffer
	r.Output, r.Diagnostics = status, &feedback
	var events []gitcache.ReceiveEvent
	r.Deliver = func(_ context.Context, event gitcache.ReceiveEvent) ([]string, error) {
		events = append(events, event)
		return nil, nil
	}
	if err := c.Receive(t.Context(), r); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("transport did not fail after refs moved", err)
	}
	want := "unprepared reference restored. Restore the receive hook and try the push again. Reference: refs/heads/second.\n"
	if !strings.Contains(status.String(), "ok refs/heads/second") || feedback.String() != want {
		t.Fatal("completion lost the restored reference notice", status.String(), feedback.String())
	}
	if got := f.Git(t, f.Store(), "rev-parse", "refs/heads/second"); got != f.Commit {
		t.Fatal("completion left the unprepared ref applied before retry", got)
	}
	if completionState(t, f) != "absent" || len(events) != 1 {
		t.Fatal("own receive did not deliver the accepted subset", events)
	}
	retryCompletion(t, c, func(_ context.Context, event gitcache.ReceiveEvent) ([]string, error) {
		events = append(events, event)
		return nil, nil
	})
	assertPreparedTransaction(t, f, events)
}
