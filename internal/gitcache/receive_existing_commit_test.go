package gitcache_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestReceiveExistingCommit(t *testing.T) {
	for _, pack := range []string{"full", "empty", "none"} {
		t.Run(pack, func(t *testing.T) {
			f := gittest.New(t)
			admitted, delivered := 0, 0
			c := receiveCache(t, f, func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error {
				admitted++
				return nil
			})
			var wire, feedback bytes.Buffer
			request := completionRequest(t, f, f.Commit)
			request.Input = existingCommitInput(t, f, pack, "")
			request.Output, request.Diagnostics = &wire, &feedback
			request.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
				delivered++
				return nil, nil
			}
			err := c.Receive(t.Context(), request)
			t.Logf("pack=%s admitted=%d delivered=%d err=%v feedback=%q wire=%q", pack, admitted, delivered, err, feedback.String(), wire.String())
			if err != nil || feedback.Len() != 0 {
				t.Fatal("existing commit transport refused", err, feedback.String())
			}
			want := [2]int{1, 1}
			if pack == "none" {
				want = [2]int{}
				if !strings.Contains(wire.String(), "ng refs/heads/topic unpacker error") {
					t.Fatal("missing pack was not refused by Git before hooks", err)
				}
			} else if f.Git(t, f.Store(), "rev-parse", "refs/heads/topic") != f.Commit {
				t.Fatal("existing commit refused", err)
			}
			if got := [2]int{admitted, delivered}; got != want {
				t.Fatal("wrong admission and delivery counts", got, want)
			}
			if completionState(t, f) != "absent" {
				t.Fatal("settled push retained a record")
			}
		})
	}
}

func TestReceiveExistingCommitRefLock(t *testing.T) {
	f := gittest.New(t)
	admitted, delivered := 0, 0
	c := receiveCache(t, f, func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error {
		admitted++
		return nil
	})
	lock := filepath.Join(f.Store(), "refs/heads/topic.lock")
	if err := os.WriteFile(lock, []byte("other writer"), 0600); err != nil {
		t.Fatal(err)
	}
	var wire, feedback bytes.Buffer
	r := completionRequest(t, f, f.Commit)
	r.Input = existingCommitInput(t, f, "empty", "")
	r.Output, r.Diagnostics = &wire, &feedback
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { delivered++; return nil, nil }
	err := c.Receive(t.Context(), r)
	if err != nil {
		t.Fatal("native refusal changed the transport result", err)
	}
	if got := [3]int{admitted, delivered, feedback.Len()}; got != [3]int{1, 0, 0} {
		t.Fatal("native refusal changed admission, delivery or feedback", got, feedback.String())
	}
	assertNativeRefLockRefusal(t, f, wire.String())
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	r.Input = existingCommitInput(t, f, "empty", "")
	wire.Reset()
	if err := c.Receive(t.Context(), r); err != nil || !strings.Contains(wire.String(), "ok refs/heads/topic") {
		t.Fatal("same push after lock release failed", err, admitted, delivered, feedback.String(), wire.String())
	}
	if got := [3]int{admitted, delivered, feedback.Len()}; got != [3]int{2, 1, 0} {
		t.Fatal("same push after lock release changed counts", got, feedback.String())
	}
}

func assertNativeRefLockRefusal(t *testing.T, f *gittest.Fixture, output string) {
	t.Helper()
	if !strings.Contains(output, "ng refs/heads/topic") {
		t.Fatal("native lock refusal was not preserved", output)
	}
	if publicReceiveRefs(t, f) != "refs/heads/main "+f.Commit || completionState(t, f) != "absent" {
		t.Fatal("native lock refusal changed refs or retained a record")
	}
	if data, err := os.ReadFile(filepath.Join(f.Store(), "refs/heads/topic.lock")); err != nil || string(data) != "other writer" {
		t.Fatal("receive removed another writer's lock", err)
	}
}

func TestReceivePublicationRefusalPreservesGateFeedback(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) { checkPublicationGateFeedback(t, partial) })
	}
}

func checkPublicationGateFeedback(t *testing.T, partial bool) {
	t.Helper()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	admitted, delivered := 0, 0
	cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error {
		admitted++
		return nil
	}
	c := thirdInit(t, cfg)
	f.Git(t, f.Bare, "update-ref", "refs/heads/topic", f.Commit)
	f.Git(t, f.Store(), "update-ref", "refs/heads/second", f.Commit)
	before := publicReceiveRefs(t, f)
	finish := pauseCompetingPublication(t, c, f, partial)
	defer finish()
	var feedback bytes.Buffer
	r := completionRequest(t, f, f.Commit)
	r.Input = existingCommitInput(t, f, "empty", "")
	if partial {
		r.Input = receiveBatchInput(t, f, []gitcache.RefUpdate{
			{Ref: "refs/heads/second", OldSHA: f.Commit},
			{Ref: "refs/heads/topic", NewSHA: f.Commit},
		}, nil)
	}
	r.Diagnostics = &feedback
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { delivered++; return nil, nil }
	err := c.Receive(t.Context(), r)
	want := "another push is in progress. Try again after the other push finishes.\n"
	counts := [2]int{admitted, delivered}
	if err == nil || counts != [2]int{1, 0} || feedback.String() != want {
		t.Fatalf("gate refusal replaced: admitted=%d delivered=%d err=%v feedback=%q refs=%q state=%q", admitted, delivered, err, feedback.String(), publicReceiveRefs(t, f), completionState(t, f))
	}
	if publicReceiveRefs(t, f) != before || completionState(t, f) != "absent" {
		t.Fatal("refused publication race did not restore refs and settle")
	}
	retryCompletion(t, c, r.Deliver)
	if delivered != 0 {
		t.Fatal("refused publication race replayed")
	}
	finish()
	r.Input = existingCommitInput(t, f, "empty", "")
	feedback.Reset()
	if err := c.Receive(t.Context(), r); err != nil || feedback.Len() != 0 {
		t.Fatal("same ref-only push after publication failed", err, feedback.String())
	}
	if got := [2]int{admitted, delivered}; got != [2]int{2, 1} {
		t.Fatal("same ref-only push after publication changed counts", got)
	}
}

func pauseCompetingPublication(t *testing.T, c *gitcache.Cache, f *gittest.Fixture, partial bool) func() {
	t.Helper()
	var prepared atomic.Int32
	var finish func()
	publication := publicationFor(f, "refs/heads/topic", "", f.Commit)
	if partial {
		gitcache.BeforeReceivePreparation(c, func() {
			if prepared.Add(1) == 2 {
				if got := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/heads/second"); got != "" {
					t.Error("first deletion did not commit", got)
				}
				finish = pauseExactPublication(t, c, f, publication)
			}
		})
	} else {
		finish = pauseExactPublication(t, c, f, publication)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if finish == nil {
				t.Error("receive did not reach the second transaction")
				return
			}
			finish()
		})
	}
}

func TestReceiveUnpreparedFeedbackDoesNotClaimHookAbsent(t *testing.T) {
	f := gittest.New(t)
	admitted, delivered := 0, 0
	c := receiveCache(t, f, func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error {
		admitted++
		return nil
	})
	var feedback bytes.Buffer
	r := completionRequest(t, f, f.Commit)
	r.Input = existingCommitInput(t, f, "empty", "")
	r.Diagnostics = &feedback
	var once sync.Once
	r.Begin = func() { once.Do(func() { changeReferenceHook(t, f, "bypassed") }) }
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { delivered++; return nil, nil }
	err := c.Receive(t.Context(), r)
	want := "push refused; refs were not prepared. Try the push again; if it repeats, ask the operator to inspect the server Git log and receive hooks.\n"
	if err == nil || admitted != 1 || delivered != 0 || feedback.String() != want {
		t.Fatal("missing preparation invented a missing admission hook", err, admitted, delivered, feedback.String())
	}
	if publicReceiveRefs(t, f) != "refs/heads/main "+f.Commit || completionState(t, f) != "absent" {
		t.Fatal("unprepared ref-only push did not roll back and settle")
	}
}

func existingCommitInput(t *testing.T, f *gittest.Fixture, pack, old string) io.ReadCloser {
	t.Helper()
	if pack == "full" {
		return receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/topic", OldSHA: old, NewSHA: f.Commit})
	}
	var body bytes.Buffer
	if old == "" {
		old = strings.Repeat("0", 40)
	}
	line := old + " " + f.Commit + " refs/heads/topic\x00report-status\n"
	_, _ = fmt.Fprintf(&body, "%04x%s0000", len(line)+4, line)
	if pack == "empty" {
		cmd := exec.CommandContext(t.Context(), f.Binary, "pack-objects", "--stdout", "--revs")
		cmd.Dir = f.Work
		data, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		if len(data) != 32 || string(data[:4]) != "PACK" || !bytes.Equal(data[8:12], []byte{0, 0, 0, 0}) {
			t.Fatal("ref-only push did not carry an empty pack")
		}
		_, _ = body.Write(data)
	}
	return io.NopCloser(bytes.NewReader(body.Bytes()))
}
