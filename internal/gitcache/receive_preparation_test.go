package gitcache_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/publicerr"
)

type referencePreparationCase struct {
	name, damage, input, refusal string
	publication                  bool
}

func TestReceiveRequiresReferencePreparation(t *testing.T) {
	unchecked := "push refused; receive gate did not run"
	busy := "another push is in progress"
	cases := []referencePreparationCase{
		{name: "intact", input: "change"},
		{name: "missing", damage: "missing", input: "change", refusal: unchecked},
		{name: "bypassed", damage: "bypassed", input: "change", refusal: unchecked},
		{name: "publication-intact", input: "change", publication: true, refusal: busy},
		{name: "publication-missing", damage: "missing", input: "change", publication: true, refusal: unchecked},
		{name: "publication-bypassed", damage: "bypassed", input: "change", publication: true, refusal: unchecked},
		{name: "unchanged-missing", damage: "missing", input: "same", refusal: unchecked},
		{name: "publication-unchanged", damage: "missing", input: "same", publication: true, refusal: unchecked},
		{name: "empty-missing", damage: "missing", input: "empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { checkReferencePreparation(t, tc) })
	}
}

func checkReferencePreparation(t *testing.T, tc referencePreparationCase) {
	t.Helper()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	admitted := 0
	cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error {
		admitted++
		return nil
	}
	c := thirdInit(t, cfg)
	f.Git(t, f.Store(), "update-ref", "refs/heads/topic", f.Commit)
	next := f.Commit
	if tc.input == "change" {
		f.Write(t, "next.txt", []byte("next\n"))
		next = f.CommitFile(t, "next")
	}
	if tc.publication {
		defer pauseReferencePublication(t, c, f)()
	}
	delivered := receiveWithReferenceHook(t, c, f, referencePush{test: tc, next: next})
	wantCalls := 1
	if tc.input == "empty" {
		wantCalls = 0
	}
	if admitted != wantCalls {
		t.Fatal("pre-receive admission changed", admitted, wantCalls)
	}
	wantDelivery := 0
	if tc.refusal == "" && tc.input != "empty" {
		wantDelivery = 1
	}
	if delivered != wantDelivery {
		t.Fatal("unchecked receive reached delivery", delivered, wantDelivery)
	}
	wantRef := f.Commit
	if tc.refusal == "" {
		wantRef = next
	}
	if got := f.Git(t, f.Store(), "rev-parse", "refs/heads/topic"); got != wantRef {
		t.Fatal("receive retained an unchecked reference", got, wantRef)
	}
}

type referencePush struct {
	test referencePreparationCase
	next string
}

func receiveWithReferenceHook(t *testing.T, c *gitcache.Cache, f *gittest.Fixture, push referencePush) int {
	t.Helper()
	input := io.NopCloser(strings.NewReader("0000"))
	if push.test.input != "empty" {
		input = receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/topic", OldSHA: f.Commit, NewSHA: push.next})
	}
	delivered := 0
	var change sync.Once
	err := c.Receive(t.Context(), gitcache.ReceiveRequest{
		Repository: gittest.Repository, Actor: "agent", Fingerprint: "key", AlwaysRun: true,
		Admission: gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: f.Commit},
		Input:     input, Output: io.Discard,
		Begin: func() {
			change.Do(func() { changeReferenceHook(t, f, push.test.damage) })
		},
		Deliver: func(context.Context, gitcache.ReceiveEvent) ([]string, error) { delivered++; return nil, nil },
	})
	if push.test.refusal == "" {
		if err != nil {
			t.Fatal("intact or unchanged receive refused", err)
		}
	} else if err == nil || !strings.Contains(err.Error(), push.test.refusal) {
		t.Fatal("receive completion did not enforce preparation", err, push.test.refusal)
	}
	if push.test.damage != "" && push.test.refusal != "" {
		t.Logf("damaged reference hook refusal (%d): %s", len(err.Error()), err)
		feedback := publicerr.Classify(err).Error()
		if feedback != "invalid_input: push refused; receive gate did not run - Restore the receive hook and try the push again." {
			t.Fatal("unchecked receive feedback changed", feedback)
		}
		t.Logf("push feedback (%d): %s", len(feedback), feedback)
	}
	return delivered
}

func changeReferenceHook(t *testing.T, f *gittest.Fixture, damage string) {
	t.Helper()
	if damage == "" {
		return
	}
	path := filepath.Join(f.Store(), "hooks", "reference-transaction")
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if damage == "bypassed" {
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
}

func pauseReferencePublication(t *testing.T, c *gitcache.Cache, f *gittest.Fixture) func() {
	t.Helper()
	reviewFault(t, f, map[string]any{"args": []string{"push"}, "stop": true})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	done := make(chan error, 1)
	joined := false
	t.Cleanup(func() {
		cancel()
		if !joined {
			<-done
		}
	})
	go func() { _, err := c.Publish(ctx, publicationFor(f, "refs/heads/topic", f.Commit, "")); done <- err }()
	pid, err := strconv.Atoi(strings.TrimSpace(string(reviewWait(t, filepath.Join(f.Root, "stopped")))))
	if err != nil {
		t.Fatal(err)
	}
	return func() {
		reviewDisarm(t, f)
		if err := resumeStopped(t, pid); err != nil {
			t.Error(err)
		}
		err := <-done
		joined = true
		if err != nil && !t.Failed() {
			t.Error("paused publication did not finish", err)
		}
	}
}
