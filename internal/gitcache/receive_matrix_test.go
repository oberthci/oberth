package gitcache_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

type receiveExitCase struct{ damage, exit string }

func TestReceiveCompletionMatrix(t *testing.T) {
	for _, damage := range []string{"intact", "missing", "empty", "not-executable", "directory", "link", "unknown"} {
		for _, shape := range []string{"change", "same", "flush", "delete", "multiple", "tag", "admission-refused", "disconnect", "cancel"} {
			t.Run(damage+"/"+shape, func(t *testing.T) { t.Parallel(); checkCompletionMatrix(t, damage, shape) })
		}
	}
}

func checkCompletionMatrix(t *testing.T, damage, shape string) {
	t.Helper()
	f := gittest.New(t)
	admitted := 0
	c := receiveCache(t, f, func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error {
		admitted++
		if shape == "admission-refused" {
			return errors.New("fixture admission refused")
		}
		return nil
	})
	for _, ref := range []string{"refs/heads/topic", "refs/heads/second"} {
		f.Git(t, f.Store(), "update-ref", ref, f.Commit)
	}
	before := publicReceiveRefs(t, f)
	f.Write(t, "matrix.txt", []byte("new source\n"))
	next := f.CommitFile(t, "matrix")
	request := completionRequest(t, f, next)
	request.Input = completionMatrixInput(t, f, shape, next)
	request.AlwaysRun = shape != "delete"
	var once sync.Once
	request.Begin = func() { once.Do(func() { damageCompletionHook(t, f, damage) }) }
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	writer := &completionStatus{failure: io.ErrClosedPipe}
	if shape == "cancel" {
		writer.cancel = cancel
		writer.failure = nil
	}
	if shape == "cancel" || shape == "disconnect" {
		request.Output = writer
	}
	delivered := 0
	request.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { delivered++; return nil, nil }
	err := c.Receive(ctx, request)
	accepted := matrixAccepted(damage, shape)
	assertMatrixRefs(t, f, before, accepted && shape != "same" && shape != "flush")
	assertMatrixExit(t, receiveExitCase{damage: damage, exit: shape}, err, writer.seen.Load())
	retryCompletion(t, c, request.Deliver)
	assertMatrixCounts(t, shape, accepted, [2]int{delivered, admitted})
}

func matrixAccepted(damage, shape string) bool {
	return damage == "intact" && shape != "admission-refused"
}

func assertMatrixRefs(t *testing.T, f *gittest.Fixture, before string, moved bool) {
	t.Helper()
	if got := publicReceiveRefs(t, f); (got != before) != moved {
		t.Errorf("reference state: %s; before: %s; want changed: %v", got, before, moved)
	}
}

func assertMatrixCounts(t *testing.T, shape string, accepted bool, got [2]int) {
	t.Helper()
	wantDelivered, wantAdmission := 0, 1
	if accepted && shape != "flush" && shape != "same" {
		wantDelivered = 1
	}
	if shape == "flush" || shape == "delete" {
		wantAdmission = 0
	}
	if got != [2]int{wantDelivered, wantAdmission} {
		t.Errorf("delivery/admission counts: got %v, want %v", got, [2]int{wantDelivered, wantAdmission})
	}
}

func completionMatrixInput(t *testing.T, f *gittest.Fixture, shape, next string) io.ReadCloser {
	t.Helper()
	updates := []gitcache.RefUpdate{{Ref: "refs/heads/topic", OldSHA: f.Commit, NewSHA: next}}
	switch shape {
	case "flush":
		return io.NopCloser(strings.NewReader("0000"))
	case "same":
		updates[0].NewSHA = f.Commit
	case "delete":
		updates[0].NewSHA = ""
	case "multiple":
		updates = append(updates, gitcache.RefUpdate{Ref: "refs/heads/second", OldSHA: f.Commit, NewSHA: next},
			gitcache.RefUpdate{Ref: "refs/heads/new/leaf", NewSHA: next})
	case "tag":
		updates = []gitcache.RefUpdate{{Ref: "refs/tags/v1.2.3", NewSHA: next}}
	}
	return receiveBatchInput(t, f, updates, nil)
}

func assertMatrixExit(t *testing.T, tc receiveExitCase, err error, triggered bool) {
	t.Helper()
	shape, damage := tc.exit, tc.damage
	switch shape {
	case "disconnect", "cancel":
		if damage != "directory" && damage != "unknown" && !triggered {
			t.Error("final status exit did not occur")
		}
		if err == nil {
			t.Error("failed transport returned success")
		}
	case "admission-refused":
		if err == nil {
			t.Error("admission refusal returned success")
		}
	default:
		if (damage == "intact" || shape == "flush") && err != nil {
			t.Error("valid receive refused", err)
		}
	}
}

func publicReceiveRefs(t *testing.T, f *gittest.Fixture) string {
	t.Helper()
	return f.Git(t, f.Store(), "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads", "refs/tags")
}

func TestFetchAfterReceiveHookInstallation(t *testing.T) {
	f := gittest.New(t)
	c := thirdInit(t, reviewConfig(t, f))
	request := completionRequest(t, f, f.Commit)
	request.Input = io.NopCloser(bytes.NewReader([]byte("0000")))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := c.Receive(ctx, request); err != nil {
		t.Fatal(err)
	}
	f.Write(t, "fetch.txt", []byte("fetched source\n"))
	next := f.CommitFile(t, "fetch")
	f.Git(t, f.Work, "push", f.Bare, "main")
	if err := c.ForceFetch(ctx, gittest.Repository); err != nil {
		t.Fatal(err)
	}
	ref, err := c.ResolveRef(ctx, gittest.Repository, "refs/heads/main")
	if err != nil || ref.ObjectSHA != next {
		t.Fatal("installed hooks blocked fetch", ref, err)
	}
}
