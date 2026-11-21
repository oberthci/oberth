package gitcache_test

import (
	"context"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func thirdWaitFile(t *testing.T, path string, limit time.Duration) bool {
	t.Helper()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		select {
		case <-timer.C:
			return false
		case <-ticker.C:
		}
	}
}
func thirdCleanupReceive(t *testing.T) (*gittest.Fixture, *gitcache.Cache, <-chan error) {
	t.Helper()

	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	_ = reviewWrapper(t, f) // builds fault.so
	root := f.Root
	wrapper := filepath.Join(root, "late-git")
	script := "#!/bin/sh\nR=" + strconv.Quote(root) + "\ncase \" $* \" in\n" +
		"*' receive-pack '*) /usr/bin/git \"$@\"; rc=$?; : > \"$R/receive-done\"; " +
		"while [ ! -e \"$R/continue\" ]; do sleep 0.02; done; exit $rc;;\n" +
		"*' update-ref '*) if [ -e \"$R/arm\" ]; then export LD_PRELOAD=\"$R/fault.so\"" +
		" REVIEW_NEEDLE=packed-refs REVIEW_PHASE=before REVIEW_MARKER=\"$R/stopped\"; fi;;\n" +
		"esac\nexec /usr/bin/git \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Binary = wrapper
	c := thirdInit(t, cfg)
	f.Git(t, f.Store(), "update-ref", "refs/heads/topic", f.Commit)
	promotion := gitport.PromotionRequest{Repository: gittest.Repository, ActionReceipt: "receipt-1",
		Source: gitport.Revision{Repository: gittest.Repository, Commit: f.Commit},
		Target: gitport.Ref{Name: "refs/heads/release"}}
	if _, err := c.PreparePromotion(t.Context(), promotion); err != nil {
		t.Fatal("prepare:", err)
	}
	f.Git(t, f.Store(), "pack-refs", "--all", "--prune")
	pushDone := make(chan error, 1)
	go func() {
		pushDone <- c.Receive(context.Background(), gitcache.ReceiveRequest{
			Repository: gittest.Repository, Actor: "agent", Fingerprint: "key",
			Admission: gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: f.Commit},
			Input:     receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/topic", OldSHA: f.Commit}),
			Output:    io.Discard,
			Deliver:   func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil }})
	}()
	return f, c, pushDone
}
func TestThirdReceiveCleanupKeepsLiveGateHolderLocks(t *testing.T) {
	t.Parallel()
	f, c, pushDone := thirdCleanupReceive(t)
	root := f.Root

	if !thirdWaitFile(t, filepath.Join(root, "receive-done"), 15*time.Second) {
		t.Fatal("native receive did not finish")
	}
	if got := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/heads/topic"); got != "" {
		t.Fatal("fixture: deletion not applied", got)
	}
	if err := os.WriteFile(filepath.Join(root, "arm"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	abandonDone := make(chan error, 1)
	go func() { abandonDone <- c.AbandonPromotion(context.Background(), gittest.Repository, "receipt-1") }()
	pid, _ := strconv.Atoi(strings.Split(string(reviewWait(t, filepath.Join(root, "stopped"))), "\n")[0])
	if err := os.Remove(filepath.Join(root, "arm")); err != nil {
		t.Fatal(err)
	}
	lock, replacement := filepath.Join(f.Store(), "packed-refs.lock"), filepath.Join(f.Store(), "packed-refs.new")
	if _, err := os.Stat(lock); err != nil {
		t.Fatal("fixture: gate holder has no packed lock:", err)
	}
	if err := os.WriteFile(filepath.Join(root, "continue"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	removed := thirdLockRemoved(lock)
	_, replacementErr := os.Stat(replacement)
	alive := syscall.Kill(pid, 0) == nil
	t.Logf("gate holder pid %d alive=%v; its packed-refs.lock removed=%v; its packed-refs.new removed=%v",
		pid, alive, removed, os.IsNotExist(replacementErr))
	_ = syscall.Kill(pid, syscall.SIGCONT)
	abandonErr := <-abandonDone
	pushErr := <-pushDone
	t.Logf("gate holder result: %v", abandonErr)
	t.Logf("push result: %v", pushErr)
	left := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/keep/")
	t.Logf("keep refs after: %q", left)
	if removed && alive {
		t.Fatal("receive cleanup deleted the lock of a live ref command that held the repository gate")
	}
}

func thirdLockRemoved(lock string) bool {
	deadline := time.NewTimer(300 * time.Millisecond)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
observe:
	for {
		select {
		case <-deadline.C:
			break observe
		case <-ticker.C:
		}
		if _, err := os.Stat(lock); os.IsNotExist(err) {
			return true
		}
	}
	return false
}
