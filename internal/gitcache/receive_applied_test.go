package gitcache_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestReceiveAppliedProcessBudget(t *testing.T) {
	for _, count := range []int{1, 16} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			f := gittest.New(t)
			cfg := reviewConfig(t, f)
			cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { return nil }
			log := filepath.Join(f.Root, "git-processes")
			cfg.Binary = filepath.Join(f.Root, "count-git")
			body := "#!/bin/sh\nprintf '%s\n' \"$*\" >> " + shellQuote(log) + "\nexec " + shellQuote(f.Binary) + " \"$@\"\n"
			if err := os.WriteFile(cfg.Binary, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			c := thirdInit(t, cfg)
			var updates []gitcache.RefUpdate
			for i := range count {
				updates = append(updates, gitcache.RefUpdate{Ref: fmt.Sprintf("refs/heads/new%d", i), NewSHA: f.Commit})
			}
			request := fourthAgentPush(t, f, updates, nil)
			if err := os.WriteFile(log, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := c.Receive(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			calls, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			got, limit := strings.Count(string(calls), "\n"), 9+2*count
			t.Logf("%d references: %d Git processes (limit %d)", count, got, limit)
			if got > limit {
				t.Fatalf("%d references: %d Git processes exceed %d\n%s", count, got, limit, calls)
			}
		})
	}
}

func TestReceiveUnchangedCommandHasNoEvent(t *testing.T) {
	f, c, next := completionFixture(t)
	f.RequireBatchedReceive(t)
	calls := 0
	push := func(old, new string) {
		r := fourthAgentPush(t, f, []gitcache.RefUpdate{{Ref: "refs/heads/topic", OldSHA: old, NewSHA: new}}, nil)
		r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { calls++; return nil, nil }
		if err := c.Receive(t.Context(), r); err != nil {
			t.Fatal(err)
		}
		retryCompletion(t, c, r.Deliver)
	}
	push(f.Commit, next)
	push(f.Commit, next)
	push(next, next)
	if calls != 1 || completionState(t, f) != "absent" {
		t.Fatal("unchanged or refused command produced another event", calls)
	}
}
