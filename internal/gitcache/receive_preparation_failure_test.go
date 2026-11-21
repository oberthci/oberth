package gitcache_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestPreparationFailurePreventsNativeCommit(t *testing.T) {
	for _, fault := range []string{"state", "write"} {
		t.Run(fault, func(t *testing.T) { checkPreparationFailure(t, fault) })
	}
}

func checkPreparationFailure(t *testing.T, fault string) {
	t.Helper()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { return nil }
	var logs []string
	cfg.Log = func(reason, detail string) { logs = append(logs, reason+": "+detail) }
	c := thirdInit(t, cfg)
	f.Git(t, f.Store(), "update-ref", "refs/heads/topic", f.Commit)
	f.Write(t, "prepare.txt", []byte("new source"))
	next := f.CommitFile(t, "prepare")
	request := completionRequest(t, f, next)
	var status, feedback bytes.Buffer
	request.Output, request.Diagnostics = &status, &feedback
	var ends atomic.Int32
	t.Cleanup(func() { _ = os.Chmod(f.Store(), 0700) })
	gitcache.BeforeReceivePreparation(c, func() {
		if ends.Add(1) == 1 {
			failPreparationStorage(t, f, fault)
		}
	})
	delivered := 0
	request.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { delivered++; return nil, nil }
	err := c.Receive(t.Context(), request)
	if err == nil || ends.Load() < 1 || strings.Contains(status.String(), "ok refs/heads/topic") {
		t.Fatal("preparation failure allowed a native commit", err, status.String())
	}
	if fault == "write" {
		assertFailingPushFeedback(t, f, feedback.String(), logs)
	}
	if err = os.Chmod(f.Store(), 0700); err != nil {
		t.Fatal(err)
	}
	retryCompletion(t, c, request.Deliver)
	if delivered != 0 || completionState(t, f) != "absent" {
		t.Fatal("preparation failure delivered", delivered)
	}
	if got := f.Git(t, f.Store(), "rev-parse", "refs/heads/topic"); got != f.Commit {
		t.Fatal("preparation failure moved ref", got)
	}
}

func failPreparationStorage(t *testing.T, f *gittest.Fixture, fault string) {
	t.Helper()
	if fault == "write" {
		if err := os.Chmod(f.Store(), 0500); err != nil {
			t.Error(err)
		}
		return
	}
	record := readCompletionRecord(t, f)
	record.State = "reserved"
	data, err := json.Marshal(record)
	if err == nil {
		err = os.WriteFile(filepath.Join(f.Store(), "oberth-receive.json"), data, 0600)
	}
	if err != nil {
		t.Error(err)
	}
}

func assertFailingPushFeedback(t *testing.T, f *gittest.Fixture, feedback string, logs []string) {
	t.Helper()
	want := "push refused; rollback is blocked. Ask the server operator to fix repository storage, then retry this push.\n"
	if !strings.HasSuffix(feedback, want) || strings.Contains(feedback, f.Store()) {
		t.Fatal("failing pusher did not get whole rollback words", feedback)
	}
	if !strings.Contains(strings.Join(logs, "\n"), f.Store()) {
		t.Fatal("native path missing from server log", logs)
	}
}
