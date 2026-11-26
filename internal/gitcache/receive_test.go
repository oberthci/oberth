package gitcache_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
)

func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "git-lost-ack" {
		os.Exit(lostAcknowledgementGit())
	}
	if os.Args[0] == "receive-test-disconnect" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := gitcache.ReceiveTestDisconnect(ctx)
		cancel()
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if code, handled := gitcache.ReceiveHookInvocation(context.Background(), os.Args, os.Stdin, os.Stderr); handled {
		os.Exit(code)
	}

	os.Exit(m.Run())
}
func receiveCache(
	t *testing.T,
	f *gittest.Fixture,
	gate func(
		context.Context,
		gitcache.ReceiveEvent,
		*gitcache.ReceiveSource,
	) error,
) *gitcache.Cache {
	t.Helper()
	return configuredReceiveCache(t, f, func(cfg *gitcache.Config) { cfg.PreReceive = gate })
}
func configuredReceiveCache(t *testing.T, f *gittest.Fixture, configure func(*gitcache.Config)) *gitcache.Cache {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := gitcache.Config{
		Root:       filepath.Join(f.Root, "cache"),
		Scratch:    filepath.Join(f.Root, "scratch"),
		Binary:     f.Binary,
		HookBinary: fastHook(t, binary),
		LiveOwners: func(context.Context, string) (gitcache.OwnerPage, error) {
			return gitcache.OwnerPage{}, nil
		},
	}
	configure(&cfg)
	c, err := gitcache.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err = c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	f.Git(t, f.Store(), "fetch", f.Bare, "+refs/heads/*:refs/heads/*")
	return c
}
func receiveInput(t *testing.T, f *gittest.Fixture, old, next string) io.ReadCloser {
	return receiveRefInput(t, f, gitcache.RefUpdate{OldSHA: old, NewSHA: next, Ref: "refs/heads/main"})
}
func receiveRefInput(t *testing.T, f *gittest.Fixture, change gitcache.RefUpdate) io.ReadCloser {
	return receiveBatchInput(t, f, []gitcache.RefUpdate{change}, nil)
}
func receiveBatchInput(t *testing.T, f *gittest.Fixture, updates []gitcache.RefUpdate, options []string) io.ReadCloser {
	t.Helper()
	var body bytes.Buffer
	var roots []string
	for i, u := range updates {
		old, next := u.OldSHA, u.NewSHA
		if old == "" {
			old = strings.Repeat("0", len(next))
		}
		if next == "" {
			next = strings.Repeat("0", len(old))
		} else {
			roots = append(roots, next)
		}
		line := old + " " + next + " " + u.Ref
		if i == 0 {
			line += "\x00report-status"
			if len(options) > 0 {
				line += " push-options"
			}
		}
		line += "\n"
		_, _ = fmt.Fprintf(&body, "%04x%s", len(line)+4, line)
	}
	_, _ = body.WriteString("0000")
	if len(options) > 0 {
		for _, option := range options {
			_, _ = fmt.Fprintf(&body, "%04x%s", len(option)+4, option)
		}
		_, _ = body.WriteString("0000")
	}
	if len(roots) > 0 {
		cmd := exec.Command(f.Binary, "pack-objects", "--stdout", "--revs")
		cmd.Dir = f.Work
		cmd.Stdin = strings.NewReader(strings.Join(roots, "\n") + "\n")
		pack, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = body.Write(pack)
	}

	return io.NopCloser(bytes.NewReader(body.Bytes()))
}

func TestReceiveReservationAndReplayPreserveAttribution(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := receiveCache(t, f, nil)
	f.Write(t, "file.txt", []byte("new source\n"))
	next := f.CommitFile(t, "push")
	attempts := 0
	var first gitcache.ReceiveEvent
	deliver := replayReader(t, c, next, &replayState{attempts: &attempts, first: &first})

	var output bytes.Buffer
	err := c.Receive(
		t.Context(),
		gitcache.ReceiveRequest{
			AllowDefault: true,
			Repository:   gittest.Repository,
			Actor:        "original-agent",
			Fingerprint:  "original-key",
			Admission:    gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: f.Commit},
			Input:        receiveInput(t, f, f.Commit, next),
			Output:       &output,
			Deliver:      deliver,
		},
	)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("receive: %v; wire %s", err, output.String())
	}
	if err = c.ReplayPending(t.Context(), deliver); err != nil {
		t.Fatal(err)
	}
	if first.Actor != "original-agent" || first.Fingerprint != "original-key" {
		t.Fatal("original receive attribution changed", first.Actor, first.Fingerprint)
	}
	if attempts != 2 || len(first.Updates) != 1 || first.Updates[0].NewSHA != next {
		t.Fatalf("%d %+v", attempts, first)
	}
	if _, err = os.Stat(filepath.Join(f.Store(), "oberth-receive.json")); !os.IsNotExist(err) {
		t.Fatal("completed reservation retained")
	}
}

type replayState struct {
	attempts *int
	first    *gitcache.ReceiveEvent
}

func replayReader(
	t *testing.T,
	c *gitcache.Cache,
	next string,
	state *replayState,
) func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
	return func(ctx context.Context, event gitcache.ReceiveEvent) ([]string, error) {
		*state.attempts++
		if state.first.ID == "" {
			*state.first = event
		} else {
			assertAttribution(t, event, *state.first)
		}
		file, err := c.ReadFile(ctx, gitport.Revision{Repository: gittest.Repository, Commit: next}, "file.txt")
		if err != nil || string(file.Bytes) != "new source\n" {
			t.Fatalf("source: %+v %v", file, err)
		}
		if *state.attempts == 1 {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, nil
	}
}
func assertAttribution(t *testing.T, got, want gitcache.ReceiveEvent) {
	t.Helper()
	if got.ID != want.ID ||
		got.Actor != want.Actor ||
		got.Fingerprint != want.Fingerprint ||
		got.Admission != want.Admission {
		t.Fatal("replay rewrote attribution")
	}
}

func TestAgentDefaultPushIsRefusedBeforeMutation(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := receiveCache(t, f, nil)
	f.Write(t, "next", []byte("test"))
	next := f.CommitFile(t, "agent push")
	delivered := false
	err := c.Receive(
		t.Context(),
		gitcache.ReceiveRequest{
			Repository:  gittest.Repository,
			Actor:       "agent",
			Fingerprint: "key",
			Admission:   gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: f.Commit},
			Input:       receiveInput(t, f, f.Commit, next),
			Output:      io.Discard,
			Deliver: func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
				delivered = true
				return nil, nil
			},
		},
	)
	if err == nil || delivered {
		t.Fatalf("agent main accepted: %v %v", err, delivered)
	}
	if got := f.Git(t, f.Store(), "rev-parse", "refs/heads/main"); got != f.Commit {
		t.Fatalf("main moved to %s", got)
	}
	if err = c.ReplayPending(t.Context(), nil); err != nil {
		t.Fatalf("rejected receive remained blocked: %v", err)
	}
}

func TestSlowReceiveDoesNotHoldSourceOrGCLock(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := receiveCache(t, f, nil)
	output := &pausedReceive{started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- c.Receive(
			t.Context(),
			gitcache.ReceiveRequest{
				Repository:  gittest.Repository,
				Actor:       "agent",
				Fingerprint: "key",
				Admission:   gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: f.Commit},
				Input:       io.NopCloser(strings.NewReader("0000")),
				Output:      output,
			},
		)
	}()
	defer func() {
		close(output.release)
		if err := <-done; err != nil {
			t.Error("empty handshake failed", err)
		}
	}()
	<-output.started
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := c.Keep(ctx, "run", f.Bindings(f.Commit)); err != nil {
		t.Error("slow receiver blocked source retention", err)
	}
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	gcLock := filepath.Join(f.Store(), "gc.pid")
	if err = os.WriteFile(gcLock, []byte(fmt.Sprintf("%d %s\n", os.Getpid(), host)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.GC(ctx); err != nil {
		t.Error("slow receiver blocked maintenance", err)
	}
	if err = os.Remove(gcLock); err != nil {
		t.Error(err)
	}
}

type pausedReceive struct {
	once             sync.Once
	started, release chan struct{}
}

func (w *pausedReceive) Write(b []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(b), nil
}
