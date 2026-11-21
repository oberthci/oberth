package gitcache_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestReceivePreparationNamesEachTransaction(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		name := "live"
		if stopped {
			name = "restart"
		}
		t.Run(name, func(t *testing.T) { checkTransactionPreparation(t, stopped) })
	}
}

func checkTransactionPreparation(t *testing.T, stopped bool) {
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
	f.Write(t, "transaction.txt", []byte("transaction source"))
	next := f.CommitFile(t, "transaction")
	r := fourthAgentPush(t, f, []gitcache.RefUpdate{
		{Ref: "refs/heads/topic", OldSHA: f.Commit},
		{Ref: "refs/heads/second", OldSHA: f.Commit, NewSHA: next},
		{Ref: "refs/heads/new", NewSHA: next},
	}, nil)
	var events []gitcache.ReceiveEvent
	deliver := func(_ context.Context, e gitcache.ReceiveEvent) ([]string, error) {
		events = append(events, e)
		return nil, nil
	}
	if stopped {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		stopTransactionReceive(t, f, cfg, r)
		record := readCompletionRecord(t, f)
		if len(record.Prepared) != 1 {
			t.Fatal("stored preparation is not the first transaction", record.Prepared)
		}
		if _, ok := record.Prepared["refs/heads/topic"]; !ok {
			t.Fatal("prepared deletion lost", record.Prepared)
		}
		c = reopenStoppedReceive(t, cfg)
	} else {
		r.Deliver = deliver
		receiveAndCheckRestoredFeedback(t, c, f, r)
	}
	for _, ref := range []string{"refs/heads/second", "refs/heads/new"} {
		if !strings.Contains(strings.Join(logs, "\n"), "unprepared reference restored: "+ref) {
			t.Fatal("restored reference was not logged", ref, logs)
		}
	}

	retryCompletion(t, c, deliver)
	assertPreparedTransaction(t, f, events)
}

func assertPreparedTransaction(t *testing.T, f *gittest.Fixture, events []gitcache.ReceiveEvent) {
	t.Helper()
	if len(events) != 1 || len(events[0].Updates) != 1 || events[0].Updates[0].Ref != "refs/heads/topic" {
		t.Fatal("unprepared transaction delivered", events)
	}
	if refs := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/heads/topic", "refs/heads/new"); refs != "" {
		t.Fatal("prepared deletion or unprepared creation wrong", refs)
	}
	if got := f.Git(t, f.Store(), "rev-parse", "refs/heads/second"); got != f.Commit {
		t.Fatal("unprepared write retained", got)
	}
	if completionState(t, f) != "absent" {
		t.Fatal("transaction did not settle")
	}
}

func stopTransactionReceive(t *testing.T, f *gittest.Fixture, cfg gitcache.Config, r gitcache.ReceiveRequest) {
	t.Helper()
	data, err := io.ReadAll(r.Input)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Input.Close()
	pack := filepath.Join(f.Root, "transaction.pack")
	if err = os.WriteFile(pack, data, 0600); err != nil {
		t.Fatal(err)
	}
	runPreparationStop(t, preparationStop{Root: cfg.Root, Scratch: cfg.Scratch, Binary: cfg.Binary, Hook: cfg.HookBinary, Store: f.Store(), Pack: pack, Commit: f.Commit, Boundary: "transport", Damage: "between"})
}

func receiveAndCheckRestoredFeedback(t *testing.T, c *gitcache.Cache, f *gittest.Fixture, r gitcache.ReceiveRequest) {
	t.Helper()
	var once sync.Once
	r.Begin = func() {
		once.Do(func() { installPreparationStop(t, preparationStop{Store: f.Store(), Damage: "between"}, 0) })
	}
	var status, feedback bytes.Buffer
	r.Output, r.Diagnostics = &status, &feedback
	if err := c.Receive(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"refs/heads/second", "refs/heads/new"} {
		if !strings.Contains(status.String(), "ok "+ref) {
			t.Fatal("probe did not receive a native ok", status.String())
		}
		want := "unprepared reference restored. Restore the receive hook and try the push again. Reference: " + ref + ".\n"
		if !strings.Contains(feedback.String(), want) {
			t.Fatal("restored reference and remedy were not sent to the pusher", feedback.String())
		}
	}
	if strings.Contains(feedback.String(), "Reference: refs/heads/topic.") {
		t.Fatal("prepared deletion reported as restored", feedback.String())
	}
}

func TestReceiveEmptyPreparationNamesRestoredReference(t *testing.T) {
	f, c, next := completionFixture(t)
	r := completionRequest(t, f, next)
	var once sync.Once
	r.Begin = func() {
		once.Do(func() { installPreparationStop(t, preparationStop{Store: f.Store(), Damage: "empty"}, 0) })
	}
	var status, feedback bytes.Buffer
	r.Output, r.Diagnostics = &status, &feedback
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
		t.Error("empty preparation delivered")
		return nil, nil
	}
	if err := c.Receive(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	want := "push refused; receive gate did not run. Restore the receive hook and try the push again. Reference: refs/heads/topic.\n"
	if !strings.Contains(status.String(), "ok refs/heads/topic") || feedback.String() != want {
		t.Fatal("native ok was not corrected with the reference and remedy", status.String(), feedback.String())
	}
	if completionState(t, f) != "absent" || f.Git(t, f.Store(), "rev-parse", "refs/heads/topic") != f.Commit {
		t.Fatal("unprepared ref did not settle")
	}
	retryCompletion(t, c, r.Deliver)
	pushAfterAbandon(t, c, f, f.Commit)
}
