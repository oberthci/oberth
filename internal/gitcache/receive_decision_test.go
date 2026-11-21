package gitcache_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestNativeReceiveExitRejectsCommittedRefs(t *testing.T) {
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { return nil }
	path := filepath.Join(f.Root, "native-exit")
	body := "#!/bin/sh\n" + shellQuote(f.Binary) + " \"$@\"\nstatus=$?\n" +
		"case \" $* \" in *' receive-pack '*) exit 128;; esac\nexit $status\n"
	if err := os.WriteFile(path, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Binary = path
	c := thirdInit(t, cfg)
	f.Git(t, f.Store(), "update-ref", "refs/heads/topic", f.Commit)
	f.Write(t, "native.txt", []byte("committed before native exit"))
	next := f.CommitFile(t, "native")
	r := completionRequest(t, f, next)
	status := &completionStatus{}
	r.Output = status
	delivered := 0
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { delivered++; return nil, nil }
	if err := c.Receive(t.Context(), r); err == nil || !status.seen.Load() {
		t.Fatal("native exit did not follow committed status", err, status.seen.Load())
	}
	assertCompletedPush(t, c, f, completionExpectation{next: next, delivered: &delivered, deliver: r.Deliver})
}

func TestReceiveUnwindCountsAsFailed(t *testing.T) {
	f, c, next := completionFixture(t)
	r := completionRequest(t, f, next)
	status := &completionStatus{}
	r.Output = status
	var once sync.Once
	r.Begin = func() { once.Do(func() { damageCompletionHook(t, f, "missing") }) }
	quarantine := filepath.Join(f.Store(), "objects", "tmp_objdir-incoming-unwind")
	gitcache.AfterReceiveTransport(c, func() {
		if status.seen.Load() {
			if err := os.Mkdir(quarantine, 0700); err != nil {
				t.Error(err)
			}
			panic("receive unwind")
		}
	})
	assertReceivePanic(t, c, r)
	if _, err := os.Stat(quarantine); !os.IsNotExist(err) {
		t.Fatal("unwind did not clean its quarantine", err)
	}
}

func TestReceiveUnwindAbortsAdmissionOnce(t *testing.T) {
	f := gittest.New(t)
	var admitted string
	var aborted []string
	c := configuredReceiveCache(t, f, func(cfg *gitcache.Config) {
		cfg.PreReceive = func(_ context.Context, e gitcache.ReceiveEvent, _ *gitcache.ReceiveSource) error {
			admitted = e.ID
			return nil
		}
		cfg.AbortReceive = func(id string) { aborted = append(aborted, id) }
	})
	f.Git(t, f.Store(), "update-ref", "refs/heads/topic", f.Commit)
	r := completionRequest(t, f, f.Commit)
	status := &completionStatus{}
	r.Output = status
	gitcache.AfterReceiveTransport(c, func() {
		if status.seen.Load() {
			panic("receive unwind")
		}
	})
	assertReceivePanic(t, c, r)
	if admitted == "" || strings.Join(aborted, ",") != admitted {
		t.Fatal("unwind must abort exactly its started admission", admitted, aborted)
	}
}

func assertReceivePanic(t *testing.T, c *gitcache.Cache, r gitcache.ReceiveRequest) {
	t.Helper()
	defer func() {
		if got := recover(); got != "receive unwind" {
			t.Error("original panic did not reach caller", got)
		}
	}()
	_ = c.Receive(t.Context(), r)
	t.Error("receive did not panic")
}
