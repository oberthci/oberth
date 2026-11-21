package gitcache_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestDeliverPushRollbackHasWholeFeedback(t *testing.T) {
	_, _, feedback := blockedDeliveryReceive(t)
	want := "receive refs need manual repair. Have the operator repair storage or restore the pending receive's before refs manually; then retry or abandon.\n"
	if feedback != want {
		t.Fatal("post-transport rollback did not report whole guidance", feedback)
	}
}

func blockedDeliveryReceive(t *testing.T) (*gittest.Fixture, *gitcache.Cache, string) {
	t.Helper()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	var logs []string
	cfg.Log = func(reason, detail string) { logs = append(logs, reason+": "+detail) }
	cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { return nil }
	c := thirdInit(t, cfg)
	for _, ref := range []string{"topic", "second"} {
		f.Git(t, f.Store(), "update-ref", "refs/heads/"+ref, f.Commit)
	}
	f.Write(t, "delivery.txt", []byte("delivery"))
	next := f.CommitFile(t, "delivery")
	r := fourthAgentPush(t, f, []gitcache.RefUpdate{{Ref: "refs/heads/topic", OldSHA: f.Commit}, {Ref: "refs/heads/second", OldSHA: f.Commit, NewSHA: next}}, nil)
	var once sync.Once
	r.Begin = func() {
		once.Do(func() { installPreparationStop(t, preparationStop{Store: f.Store(), Damage: "between"}, 0) })
	}
	status := &completionStatus{}
	var feedback bytes.Buffer
	r.Output, r.Diagnostics = status, &feedback
	heads := filepath.Join(f.Store(), "refs", "heads")
	t.Cleanup(func() { _ = os.Chmod(heads, 0700) })
	gitcache.AfterReceiveTransport(c, func() {
		if status.seen.Load() {
			if err := os.Chmod(heads, 0500); err != nil {
				t.Error(err)
			}
		}
	})
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
		t.Error("blocked restoration reached delivery")
		return nil, nil
	}
	err := c.Receive(t.Context(), r)
	var reported interface{ Reported() bool }
	if err == nil || !errors.As(err, &reported) || !reported.Reported() {
		t.Fatal("deliverPush failure was not reported", err)
	}
	for _, ref := range []string{"topic", "second"} {
		if !strings.Contains(status.String(), "ok refs/heads/"+ref) {
			t.Fatal("transport did not succeed before rollback failed", status.String())
		}
	}
	if strings.Contains(feedback.String(), f.Store()) || !strings.Contains(strings.Join(logs, "\n"), f.Store()) {
		t.Fatal("native detail was not confined to server logs", feedback.String(), logs)
	}
	return f, c, feedback.String()
}

func TestDeliverPushRepairDoesNotDeliverARefusal(t *testing.T) {
	f, c, feedback := blockedDeliveryReceive(t)
	if strings.Contains(feedback, "push refused") {
		t.Fatal("pending prepared work was described as refused", feedback)
	}
	record, err := c.PendingReceive(t.Context(), gittest.Repository)
	if err != nil || record.State != "ready" || len(record.Prepared) != 1 {
		t.Fatal("prepared subset was not preserved for repair", record, err)
	}
	if err = os.Chmod(filepath.Join(f.Store(), "refs", "heads"), 0700); err != nil {
		t.Fatal(err)
	}
	var events []gitcache.ReceiveEvent
	retryCompletion(t, c, func(_ context.Context, event gitcache.ReceiveEvent) ([]string, error) {
		events = append(events, event)
		return nil, nil
	})
	if len(events) != 1 || events[0].ID != record.Event.ID || len(events[0].Updates) != 1 || events[0].Updates[0].Ref != "refs/heads/topic" || events[0].Updates[0].NewSHA != "" {
		t.Fatal("delivery included an unprepared reference", events)
	}
	assertPreparedTransaction(t, f, events)
}
