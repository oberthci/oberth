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

func TestRejectionWriteFailureStillRestoresRefs(t *testing.T) {
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { return nil }
	arm := filepath.Join(f.Root, "restore-record-directory")
	wrapper := filepath.Join(f.Root, "git-storage")
	body := "#!/bin/sh\nif [ -e " + shellQuote(arm) + " ]; then chmod 700 " + shellQuote(f.Store()) +
		"; fi\nexec " + shellQuote(f.Binary) + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Binary = wrapper
	c := thirdInit(t, cfg)
	f.Git(t, f.Store(), "update-ref", "refs/heads/topic", f.Commit)
	f.Write(t, "storage.txt", []byte("unchecked source"))
	next := f.CommitFile(t, "storage")
	r := completionRequest(t, f, next)
	status := &completionStatus{}
	r.Output = status
	var once sync.Once
	r.Begin = func() { once.Do(func() { damageCompletionHook(t, f, "missing") }) }
	t.Cleanup(func() { _ = os.Chmod(f.Store(), 0700) })
	gitcache.AfterReceiveTransport(c, func() {
		if status.seen.Load() {
			if err := os.WriteFile(arm, nil, 0600); err != nil {
				t.Error(err)
			}
			if err := os.Chmod(f.Store(), 0500); err != nil {
				t.Error(err)
			}
		}
	})
	delivered := 0
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { delivered++; return nil, nil }
	err := c.Receive(t.Context(), r)
	if !errors.Is(err, os.ErrPermission) {
		t.Fatal("rejection persistence failure was lost", err)
	}
	assertCompletedPush(t, c, f, completionExpectation{next: next, delivered: &delivered, deliver: r.Deliver})
	if completionState(t, f) != "absent" {
		t.Fatal("restored receive still blocks its repository")
	}
}

func TestRollbackFailureShowsWholeFeedbackAndLogsNativeDetail(t *testing.T) {
	var logs []string
	r := failedRollback(t, func(cfg *gitcache.Config) {
		cfg.Log = func(reason, detail string) { logs = append(logs, reason+": "+detail) }
	})
	if err := os.Remove(filepath.Join(r.f.Root, "arm")); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(r.f.Store(), "refs", "heads", "topic.lock")
	if err := os.WriteFile(lock, []byte("another writer"), 0600); err != nil {
		t.Fatal(err)
	}
	request := fourthAgentPush(t, r.f, []gitcache.RefUpdate{{Ref: "refs/heads/later", NewSHA: r.unchecked}}, nil)
	var feedback bytes.Buffer
	request.Diagnostics = &feedback
	err := r.c.Receive(t.Context(), request)
	assertReceiveRollbackFeedback(t, err, feedback.String())
	if strings.Contains(feedback.String(), r.f.Store()) || !strings.Contains(strings.Join(logs, "\n"), r.f.Store()) {
		t.Fatal("native server path was lost or sent to the pusher", feedback.String(), logs)
	}
	if data, readErr := os.ReadFile(lock); readErr != nil || string(data) != "another writer" {
		t.Fatal("another writer's lock was removed", readErr)
	}
	if err = os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	calls := 0
	retryCompletion(t, r.c, func(context.Context, gitcache.ReceiveEvent) ([]string, error) { calls++; return nil, nil })
	if calls != 0 {
		t.Fatal("rejected event delivered", calls)
	}
}

func assertReceiveRollbackFeedback(t *testing.T, err error, feedback string) {
	t.Helper()
	var reported interface{ Reported() bool }
	want := "push refused; rollback is blocked. Ask the server operator to fix repository storage, then retry this push.\n"
	if !errors.As(err, &reported) || !reported.Reported() || feedback != want {
		t.Fatal("rollback feedback is not whole and reported", err, feedback)
	}
}
