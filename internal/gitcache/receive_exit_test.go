package gitcache_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func completionFixture(t *testing.T) (*gittest.Fixture, *gitcache.Cache, string) {
	t.Helper()
	f := gittest.New(t)
	c := receiveCache(t, f, func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { return nil })
	f.Git(t, f.Store(), "update-ref", "refs/heads/topic", f.Commit)
	f.Write(t, "exit.txt", []byte("new source\n"))
	return f, c, f.CommitFile(t, "exit")
}

func completionRequest(t *testing.T, f *gittest.Fixture, next string) gitcache.ReceiveRequest {
	t.Helper()
	return gitcache.ReceiveRequest{
		Repository: gittest.Repository, Actor: "agent", Fingerprint: "key", AlwaysRun: true,
		Admission: gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: f.Commit},
		Input:     receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/topic", OldSHA: f.Commit, NewSHA: next}),
		Output:    io.Discard, Diagnostics: io.Discard,
	}
}

func retryCompletion(t *testing.T, c *gitcache.Cache, deliver func(context.Context, gitcache.ReceiveEvent) ([]string, error)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for range 2 {
		if err := c.RetryReceive(ctx, gittest.Repository, deliver); err != nil {
			t.Fatal("retry did not finish", err)
		}
	}
}

func completionState(t *testing.T, f *gittest.Fixture) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.Store(), "oberth-receive.json"))
	if os.IsNotExist(err) {
		return "absent"
	}
	if err != nil {
		t.Fatal(err)
	}
	var record gitcache.Reservation
	if err = json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record.State
}

type completionStatus struct {
	bytes.Buffer
	seen           atomic.Bool
	failure        error
	failureAfter   string
	ctx            context.Context
	ready, release chan struct{}
	cancel         context.CancelFunc
	once           sync.Once
}

func (w *completionStatus) Write(data []byte) (int, error) {
	n, _ := w.Buffer.Write(data)
	if bytes.Contains(w.Bytes(), []byte("unpack ok")) {
		fresh := !w.seen.Swap(true)
		if w.ready != nil {
			w.once.Do(func() { close(w.ready) })
		}
		if w.release != nil {
			select {
			case <-w.release:
			case <-w.ctx.Done():
				return 0, context.Cause(w.ctx)
			}
		}
		if fresh && w.cancel != nil {
			w.cancel()
			return n, nil
		}
		if w.failure != nil && (w.failureAfter == "" || bytes.Contains(w.Bytes(), []byte(w.failureAfter))) {
			return 0, w.failure
		}
	}
	return n, nil
}

func TestReceiveTransportUnwinding(t *testing.T) {
	for _, damage := range []string{"intact", "missing"} {
		for _, exit := range []string{"panic", "cancel", "unexpected"} {
			t.Run(damage+"/"+exit, func(t *testing.T) { checkTransportUnwinding(t, damage, exit) })
		}
	}
}

func checkTransportUnwinding(t *testing.T, damage, exit string) {
	t.Helper()
	f, c, next := completionFixture(t)
	request := completionRequest(t, f, next)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	status := &completionStatus{}
	request.Output = status
	if exit == "unexpected" {
		status.failure = errors.New("fixture transport error")
	}
	var once sync.Once
	request.Begin = func() { once.Do(func() { damageCompletionHook(t, f, damage) }) }
	gitcache.AfterReceiveTransport(c, func() {
		if status.seen.Load() {
			switch exit {
			case "panic":
				panic("fixture transport panic")
			case "cancel":
				cancel()
			}
		}
	})
	delivered := 0
	request.Deliver = func(ctx context.Context, _ gitcache.ReceiveEvent) ([]string, error) {
		if ctx.Err() != nil {
			t.Error("transport cancelled accepted delivery", ctx.Err())
		}
		delivered++
		return nil, nil
	}
	var err error
	var panicked any
	func() {
		defer func() { panicked = recover() }()
		err = c.Receive(ctx, request)
	}()
	assertTransportExit(t, damage, exit, transportExit{seen: status.seen.Load(), delivered: delivered, panicked: panicked, err: err})
	assertCompletedPush(t, c, f, completionExpectation{next: next, accepted: damage == "intact", delivered: &delivered, deliver: request.Deliver})
}

type transportExit struct {
	seen      bool
	delivered int
	panicked  any
	err       error
}

func assertTransportExit(t *testing.T, damage, exit string, result transportExit) {
	t.Helper()
	if !result.seen {
		t.Fatal("exit control did not follow native commit")
	}
	wantDelivered := 0
	if damage == "intact" {
		wantDelivered = 1
	}
	if result.delivered != wantDelivered {
		t.Fatal("own receive did not deliver its accepted push", result.delivered, wantDelivered)
	}
	if exit == "panic" && result.panicked != "fixture transport panic" {
		t.Error("original panic lost", result.panicked)
	}
	if exit != "panic" && (result.panicked != nil || result.err == nil) {
		t.Error("transport failure lost", result.panicked, result.err)
	}
}

type completionExpectation struct {
	next      string
	accepted  bool
	delivered *int
	deliver   func(context.Context, gitcache.ReceiveEvent) ([]string, error)
}

func assertCompletedPush(t *testing.T, c *gitcache.Cache, f *gittest.Fixture, want completionExpectation) {
	t.Helper()
	ref, deliveries := f.Commit, 0
	if want.accepted {
		ref, deliveries = want.next, 1
	}
	if got := f.Git(t, f.Store(), "rev-parse", "refs/heads/topic"); got != ref {
		t.Error("completion left wrong ref", got, ref)
	}
	retryCompletion(t, c, want.deliver)
	if *want.delivered != deliveries {
		t.Errorf("completion delivered %d events, want %d", *want.delivered, deliveries)
	}
}

func TestReceiveCommittedAcknowledgementDisconnect(t *testing.T) {
	for _, damage := range []string{"intact", "bypassed"} {
		t.Run(damage, func(t *testing.T) { checkCommittedAcknowledgement(t, damage, false) })
	}
}

func checkCommittedAcknowledgement(t *testing.T, damage string, gone bool) {
	t.Helper()
	f := gittest.New(t)
	var logged []string
	aborted := 0
	c := configuredReceiveCache(t, f, func(cfg *gitcache.Config) {
		cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { return nil }
		cfg.Log = func(reason, detail string) { logged = append(logged, reason+": "+detail) }
		cfg.AbortReceive = func(string) { aborted++ }
	})
	f.Git(t, f.Store(), "update-ref", "refs/heads/topic", f.Commit)
	f.Write(t, "ack.txt", []byte("accepted source"))
	next := f.CommitFile(t, "ack")
	request := completionRequest(t, f, next)
	diagnostics := ackDiagnostics{gone: gone}
	request.Diagnostics = &diagnostics
	var once sync.Once
	request.Begin = func() { once.Do(func() { disconnectCommittedHook(t, f, damage) }) }
	delivered := 0
	request.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { delivered++; return nil, nil }
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err := c.Receive(ctx, request)
	if damage == "intact" {
		if err != nil || delivered != 1 || aborted != 0 {
			t.Fatal("accepted push was not delivered immediately without abort", err, delivered, aborted)
		}
		checkAcknowledgementFeedback(t, diagnostics, logged)

	} else if !errors.Is(err, syscall.EPIPE) || delivered != 0 {
		t.Fatal("bypassed preparation was accepted", err, delivered)
	}
	if completionState(t, f) != "absent" {
		t.Fatal("completed receive retained a record")
	}
	assertCompletedPush(t, c, f, completionExpectation{next: next, accepted: damage == "intact", delivered: &delivered, deliver: request.Deliver})
}

func TestReceiveCommittedAcknowledgementWithClosedDiagnostics(t *testing.T) {
	checkCommittedAcknowledgement(t, "intact", true)
}

type ackDiagnostics struct {
	bytes.Buffer
	gone bool
}

func (w *ackDiagnostics) WriteString(text string) (int, error) {
	return w.Write([]byte(text))
}

func (w *ackDiagnostics) Write(data []byte) (int, error) {
	if w.gone && bytes.Contains(data, []byte("push accepted.")) {
		return 0, io.ErrClosedPipe
	}
	return w.Buffer.Write(data)
}

func checkAcknowledgementFeedback(t *testing.T, diagnostics ackDiagnostics, logged []string) {
	t.Helper()
	want := "push accepted.\n"
	if diagnostics.gone {
		want = ""
		if !strings.Contains(strings.Join(logged, "\n"), io.ErrClosedPipe.Error()) {
			t.Fatal("closed diagnostics failure was not logged", logged)
		}
	}
	if diagnostics.String() != want || !strings.Contains(strings.Join(logged, "\n"), "broken pipe") {
		t.Fatal("accepted acknowledgement feedback or native failure lost", diagnostics.String(), logged)
	}
}

func disconnectCommittedHook(t *testing.T, f *gittest.Fixture, damage string) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	changeReferenceHook(t, f, "missing")
	body := "#!/usr/bin/python3\nimport os,sys\nos.environ['GORACE']='atexit_sleep_ms=0'\n" +
		"if " + strconv.Quote(damage) + "=='bypassed' and sys.argv[1]!='committed': sys.exit(0)\n" +
		"name='receive-test-disconnect' if sys.argv[1]=='committed' else 'reference-transaction'\n" +
		"os.execv(" + strconv.Quote(binary) + ",[name,sys.argv[1]])\n"
	if err := os.WriteFile(filepath.Join(f.Store(), "hooks", "reference-transaction"), []byte(body), 0700); err != nil {
		t.Error(err)
	}
}
