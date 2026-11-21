package gitcache_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
)

func TestR2CrashDuringReconciliation(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	for _, phase := range []string{"before", "after"} {
		t.Run(phase, func(t *testing.T) {
			f := gittest.New(t)
			cfg := reviewConfig(t, f)
			cfg.Binary = reviewWrapper(t, f)
			c := reviewNew(t, cfg)
			if err := c.Init(t.Context(), gittest.Repository); err != nil {
				t.Fatal(err)
			}
			f.Write(t, "next", []byte("next"))
			next := f.CommitFile(t, "next")
			f.Git(t, f.Work, "push", f.Bare, "main")
			reviewFault(t, f, map[string]any{"args": []string{"update-ref", "--no-deref"},
				"needle": "refs/oberth/upstream/heads/main", "phase": "after"})
			done := make(chan error, 1)
			go func() { done <- c.ForceFetch(t.Context(), gittest.Repository) }()
			reviewKill(t, f)
			<-done
			reviewDisarm(t, f)
			_ = c.Close()
			if err := os.Remove(filepath.Join(f.Root, "stopped")); err != nil {
				t.Fatal(err)
			}
			reviewFault(t, f, map[string]any{"args": []string{"update-ref"}, "needle": "refs/heads/main", "phase": phase})
			go func() {
				opened, err := gitcache.New(t.Context(), cfg)
				if opened != nil {
					_ = opened.Close()
				}
				done <- err
			}()
			reviewKill(t, f)
			t.Log("interrupted recovery:", <-done)
			reviewDisarm(t, f)
			c = reviewNew(t, cfg)
			public := f.Git(t, f.Store(), "rev-parse", "refs/heads/main")
			mirror := f.Git(t, f.Store(), "rev-parse", "refs/oberth/upstream/heads/main")
			if public != next || mirror != next {
				t.Fatal("second recovery lost consistency", public, mirror, next)
			}
			if _, err := os.Stat(filepath.Join(f.Store(), "oberth-reconcile")); !os.IsNotExist(err) {
				t.Fatal("marker not cleared", err)
			}
			f.Git(t, f.Store(), "fsck", "--full")
		})
	}
}

func r2RefTransaction(t *testing.T, dir, body string) {
	t.Helper()
	cmd := exec.Command("/usr/bin/git", "update-ref", "--stdin")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(body)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ref fixture: %v %s", err, out)
	}
}
func TestR2ReconciliationTurnoverBound(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	f := gittest.New(t)
	var seed strings.Builder
	for i := 0; i < 2049; i++ {
		fmt.Fprintf(&seed, "update refs/heads/old%04d %s\n", i, f.Commit)
	}
	r2RefTransaction(t, f.Bare, seed.String())
	cfg := reviewConfig(t, f)
	wrapper := filepath.Join(f.Root, "turnover-git")
	// Only the final mirror publication stops; all inventory reads use native Git.
	script := "#!/bin/sh\ncase \" $* \" in\n*' update-ref '*' --no-deref '*) echo $$ > " +
		strconv.Quote(filepath.Join(f.Root, "stopped")) +
		"; kill -STOP $$;;\nesac\nexec /usr/bin/git \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Binary = wrapper
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	var replace strings.Builder
	for i := 0; i < 2049; i++ {
		fmt.Fprintf(&replace, "delete refs/heads/old%04d\nupdate refs/heads/new%04d %s\n", i, i, f.Commit)
	}
	r2RefTransaction(t, f.Bare, replace.String())
	done := make(chan error, 1)
	go func() { done <- c.ForceFetch(t.Context(), gittest.Repository) }()
	// This fixture has 4,098 changed names, although each inventory has only 2,050 refs.
	waitMirrorStop(t, f, done)
	data, err := os.ReadFile(filepath.Join(f.Store(), "oberth-reconcile"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("marker has %d names, %d bytes; before and after are 2050 refs each",
		len(strings.Fields(string(data))), len(data))
	reviewKill(t, f)
	t.Log("interrupted turnover:", <-done)
	_ = c.Close()
	cfg.Binary = "/usr/bin/git"
	reopened, err := gitcache.New(t.Context(), cfg)
	if reopened != nil {
		defer reopened.Close()
	}
	if err != nil {
		t.Fatal("valid-sized inventory turnover prevents cache reopening:", err)
	}
}

func TestR2TagCreatedDuringPublishIsAlreadyExists(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	f.Write(t, "next", []byte("next"))
	next := f.CommitFile(t, "next")
	f.Git(t, f.Bare, "fetch", f.Work, next)
	ref := "refs/tags/racing"
	f.Git(t, f.Store(), "update-ref", ref, f.Commit)
	reviewFault(t, f, map[string]any{"args": []string{"push"}, "stop": true})
	done := make(chan error, 1)
	go func() { _, err := c.Publish(t.Context(), publicationFor(f, ref, f.Commit, "")); done <- err }()
	pid, _ := strconv.Atoi(strings.TrimSpace(string(reviewWait(t, filepath.Join(f.Root, "stopped")))))
	f.Git(t, f.Bare, "update-ref", ref, next)
	reviewDisarm(t, f)
	if err := resumeStopped(t, pid); err != nil {
		t.Fatal(err)
	}
	err := <-done
	var exists *gitport.AlreadyExistsError
	if !errors.As(err, &exists) {
		t.Fatalf("concurrent tag creation classified differently from preflight: %T %v", err, err)
	}
}

func TestR2PackedFetchCrashReopens(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	f := gittest.New(t)
	f.Git(t, f.Bare, "update-ref", "refs/heads/topic", f.Commit)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	f.Git(t, f.Store(), "pack-refs", "--all", "--prune")
	f.Git(t, f.Bare, "update-ref", "-d", "refs/heads/topic")
	reviewFault(t, f, map[string]any{"args": []string{"update-ref", "--no-deref"},
		"needle": "packed-refs", "phase": "before"})
	done := make(chan error, 1)
	go func() { done <- c.ForceFetch(t.Context(), gittest.Repository) }()
	reviewKill(t, f)
	t.Log("interrupted packed fetch:", <-done)
	reviewDisarm(t, f)
	_ = c.Close()
	reopened, err := gitcache.New(t.Context(), cfg)
	if reopened != nil {
		defer reopened.Close()
	}
	if err != nil {
		t.Fatal("one interrupted packed fetch prevents whole cache open:", err)
	}
	if err = reopened.ForceFetch(t.Context(), gittest.Repository); err != nil {
		t.Fatal("packed fetch retry after open still fails:", err)
	}
}

func TestR2TransientMirrorCountAtLimit(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	f := gittest.New(t)
	var seed strings.Builder
	for i := 0; i < 4095; i++ {
		fmt.Fprintf(&seed, "update refs/heads/old%04d %s\n", i, f.Commit)
	}
	r2RefTransaction(t, f.Bare, seed.String())
	cfg := reviewConfig(t, f)
	_ = reviewWrapper(t, f) // Build the native rename stop fixture.
	wrapper := filepath.Join(f.Root, "cap-git")
	script := "#!/bin/sh\ncase \" $* \" in\n*' update-ref '*' --no-deref '*) export LD_PRELOAD=" +
		strconv.Quote(filepath.Join(f.Root, "fault.so")) +
		" REVIEW_NEEDLE=refs/oberth/upstream/heads/aaa REVIEW_PHASE=after REVIEW_MARKER=" +
		strconv.Quote(filepath.Join(f.Root, "stopped")) +
		";;\nesac\nexec /usr/bin/git \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Binary = wrapper
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	r2RefTransaction(t, f.Bare, "delete refs/heads/old4094\nupdate refs/heads/aaa "+f.Commit+"\n")
	done := make(chan error, 1)
	go func() { done <- c.ForceFetch(t.Context(), gittest.Repository) }()
	waitMirrorStop(t, f, done)
	reviewKill(t, f)
	t.Log("interrupted at cap:", <-done)
	mirrors := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/upstream/")
	t.Logf("interrupted tracking count=%d", len(strings.Fields(mirrors)))
	_ = c.Close()
	cfg.Binary = "/usr/bin/git"
	reopened, err := gitcache.New(t.Context(), cfg)
	if reopened != nil {
		defer reopened.Close()
	}
	if err != nil {
		t.Fatal("within-cap snapshots cannot reopen after one ref replacement:", err)
	}
	if err = reopened.ForceFetch(t.Context(), gittest.Repository); err != nil {
		t.Fatal("next fetch cannot consume interrupted tracking", err)
	}
	got := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/upstream/")
	if len(strings.Fields(got)) != 4096 {
		t.Fatal("tracking was not refreshed")
	}

}

func waitMirrorStop(t *testing.T, f *gittest.Fixture, done <-chan error) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Minute)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if b, err := os.ReadFile(filepath.Join(f.Root, "stopped")); err == nil && len(b) > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatal("fetch did not reach marker", err)
		case <-deadline.C:
			t.Fatal("turnover fixture did not finish inventory")
		case <-tick.C:
		}
	}
}

func TestPackedReceiveDeletionRetriesAfterKilledReplacement(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	f.Git(t, f.Store(), "update-ref", "refs/heads/topic", f.Commit)
	f.Git(t, f.Store(), "pack-refs", "--all", "--prune")
	request := func() gitcache.ReceiveRequest {
		return gitcache.ReceiveRequest{Repository: gittest.Repository, Actor: "actor", Fingerprint: "key",
			Admission: gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: f.Commit},
			Input:     receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/topic", OldSHA: f.Commit}),
			Output:    io.Discard,
			Deliver:   func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil }}
	}
	reviewFault(t, f, map[string]any{"args": []string{"receive-pack"}, "needle": "packed-refs", "phase": "before"})
	done := make(chan error, 1)
	go func() { done <- c.Receive(t.Context(), request()) }()
	reviewKill(t, f)
	if err := <-done; err == nil {
		t.Fatal("killed receive succeeded")
	}
	reviewDisarm(t, f)
	if err := c.Receive(t.Context(), request()); err != nil {
		t.Fatal("packed deletion retry", err)
	}
	if got := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/heads/topic"); got != "" {
		t.Fatal("deleted branch survived", got)
	}
}
