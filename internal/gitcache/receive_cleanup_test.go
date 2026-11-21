package gitcache_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestExpiredReceiveCleanupDoesNotBlockNextOperation(t *testing.T) {
	for _, operation := range []string{"fetch", "push"} {
		t.Run(operation, func(t *testing.T) { checkExpiredReceiveCleanup(t, operation) })
	}
}

func checkExpiredReceiveCleanup(t *testing.T, operation string) {
	t.Helper()
	r := expiredReceiveCleanup(t, nil)
	checkCleanupNextOperation(t, r.c, completionRequest(t, r.f, r.next), operation)
	checkReceiveDebrisRemoved(t, r.f, []string{r.lock, r.quarantine}, r.foreign)
}

type cleanupFixture struct {
	f                               *gittest.Fixture
	c                               *gitcache.Cache
	next, lock, quarantine, foreign string
}

func expiredReceiveCleanup(t *testing.T, log func(string, string)) cleanupFixture {
	t.Helper()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Log = log
	cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { return nil }
	arm := filepath.Join(f.Root, "fail-receive")
	wrapper := filepath.Join(f.Root, "git-exit")
	// The marker affects only this receive's native return, after references commit.
	body := "#!/bin/sh\n" + shellQuote(f.Binary) + " \"$@\"\nstatus=$?\n" +
		"case \" $* \" in *' receive-pack '*) if [ -e " + shellQuote(arm) + " ]; then exit 128; fi;; esac\nexit $status\n"
	if err := os.WriteFile(wrapper, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Binary = wrapper
	c := thirdInit(t, cfg)
	f.Git(t, f.Store(), "update-ref", "refs/heads/topic", f.Commit)
	f.Write(t, "cleanup.txt", []byte("committed source"))
	next := f.CommitFile(t, "cleanup")
	foreign := filepath.Join(f.Store(), "refs", "heads", "foreign.lock")
	if err := os.WriteFile(foreign, []byte("another writer"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(arm, nil, 0600); err != nil {
		t.Fatal(err)
	}
	r := completionRequest(t, f, next)
	status := &completionStatus{}
	r.Output = status
	gitcache.SetReceiveCleanupBudget(c, time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var release func()
	t.Cleanup(func() {
		if release != nil {
			release()
		}
	})
	lock := filepath.Join(f.Store(), "refs", "heads", "topic.lock")
	quarantine := filepath.Join(f.Store(), "objects", "tmp_objdir-incoming-cleanup")
	gitcache.AfterReceiveTransport(c, func() {
		if status.seen.Load() {
			release = holdReceiveDebris(t, ctx, c, f.Store())
		}
	})
	if err := c.Receive(ctx, r); !errors.Is(err, context.DeadlineExceeded) || release == nil {
		t.Fatal("cleanup did not expire behind the repository gate", err)
	}
	if completionState(t, f) != "rejected" {
		t.Fatal("deadline lost rejection")
	}
	release()
	release = nil
	if err := os.Remove(arm); err != nil {
		t.Fatal(err)
	}
	return cleanupFixture{f, c, next, lock, quarantine, foreign}
}

func holdReceiveDebris(t *testing.T, ctx context.Context, c *gitcache.Cache, store string) func() {
	t.Helper()
	// The committed ref retains the inode Git owned as its transaction lock.
	ref := filepath.Join(store, "refs", "heads", "topic")
	if err := os.Link(ref, ref+".lock"); err != nil {
		t.Error(err)
	}
	if err := os.Mkdir(filepath.Join(store, "objects", "tmp_objdir-incoming-cleanup"), 0700); err != nil {
		t.Error(err)
	}
	release, err := gitcache.HoldReceiveRepository(ctx, c, gittest.Repository)
	if err != nil {
		t.Error(err)
	}
	return release
}

func checkCleanupNextOperation(t *testing.T, c *gitcache.Cache, r gitcache.ReceiveRequest, operation string) {
	t.Helper()
	if operation == "fetch" {
		if err := c.ForceFetch(t.Context(), gittest.Repository); err != nil {
			t.Fatal("finished receive blocked fetch", err)
		}
		return
	}
	delivered := 0
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { delivered++; return nil, nil }
	if err := c.Receive(t.Context(), r); err != nil || delivered != 1 {
		t.Fatal("finished receive blocked next push", err, delivered)
	}
}

func checkReceiveDebrisRemoved(t *testing.T, f *gittest.Fixture, owned []string, foreign string) {
	t.Helper()
	for _, path := range owned {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Error("owned receive resource remains", path, err)
		}
	}
	if data, err := os.ReadFile(foreign); err != nil || string(data) != "another writer" {
		t.Fatal("cleanup removed another writer's lock", err)
	}
	if completionState(t, f) != "absent" {
		t.Fatal("finished record remains")
	}
}

func TestReceiveCleanupFailureHasWholeFeedback(t *testing.T) {
	var logs []string
	r := expiredReceiveCleanup(t, func(reason, detail string) { logs = append(logs, reason+": "+detail) })
	dir := filepath.Dir(r.lock)
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	request := completionRequest(t, r.f, r.next)
	var feedback bytes.Buffer
	request.Diagnostics = &feedback
	err := r.c.Receive(t.Context(), request)
	assertReceiveRollbackFeedback(t, err, feedback.String())
	if strings.Contains(feedback.String(), r.f.Store()) || !strings.Contains(strings.Join(logs, "\n"), r.lock) {
		t.Fatal("owned cleanup leaked or lost native failure detail", feedback.String(), logs)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	calls := 0
	retryCompletion(t, r.c, func(context.Context, gitcache.ReceiveEvent) ([]string, error) { calls++; return nil, nil })
	if calls != 0 {
		t.Fatal("cleanup failure delivered a rejected receive", calls)
	}
	checkReceiveDebrisRemoved(t, r.f, []string{r.lock, r.quarantine}, r.foreign)
}

func TestLiveReceiveCleanupFailureHasWholeFeedback(t *testing.T) {
	f := gittest.New(t)
	var logs []string
	c := configuredReceiveCache(t, f, func(cfg *gitcache.Config) {
		cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { return nil }
		cfg.Log = func(reason, detail string) { logs = append(logs, reason+": "+detail) }
	})
	f.Git(t, f.Store(), "update-ref", "refs/heads/topic", f.Commit)
	f.Write(t, "live-cleanup.txt", []byte("committed source"))
	next := f.CommitFile(t, "live cleanup")
	r := completionRequest(t, f, next)
	status := &completionStatus{failure: io.ErrClosedPipe}
	r.Output = status
	var feedback bytes.Buffer
	r.Diagnostics = &feedback
	objects := filepath.Join(f.Store(), "objects")
	quarantine := filepath.Join(objects, "tmp_objdir-incoming-live-cleanup")
	t.Cleanup(func() { _ = os.Chmod(objects, 0700) })
	gitcache.AfterReceiveTransport(c, func() {
		if status.seen.Load() {
			if err := os.Mkdir(quarantine, 0700); err != nil {
				t.Error(err)
			}
			if err := os.Chmod(objects, 0500); err != nil {
				t.Error(err)
			}
		}
	})
	calls := 0
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { calls++; return nil, nil }
	err := c.Receive(t.Context(), r)
	assertReceiveCleanupPendingFeedback(t, err, feedback.String())
	if strings.Contains(feedback.String(), f.Store()) || !strings.Contains(strings.Join(logs, "\n"), quarantine) {
		t.Fatal("live cleanup leaked or lost its native path", feedback.String(), logs)
	}
	if err = os.Chmod(objects, 0700); err != nil {
		t.Fatal(err)
	}
	assertCompletedPush(t, c, f, completionExpectation{next: next, accepted: true, delivered: &calls, deliver: r.Deliver})
	if _, err = os.Stat(quarantine); !os.IsNotExist(err) {
		t.Fatal("retry retained owned quarantine", err)
	}
}

func assertReceiveCleanupPendingFeedback(t *testing.T, err error, feedback string) {
	t.Helper()
	var reported interface{ Reported() bool }
	want := "push saved; run delivery is pending. Ask the server operator to clear this receive's owned debris, then retry delivery.\n"
	if !errors.As(err, &reported) || !reported.Reported() || feedback != want {
		t.Fatal("accepted cleanup was reported as refusal or lost its remedy", err, feedback)
	}
}
