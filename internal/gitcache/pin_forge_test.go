package gitcache_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

// silentForge makes every Git command that names an armed fixture forge
// wait for a line the test writes only at release or cleanup: a forge that
// does not answer within any caller's deadline. After holdOnly only its
// commands that carry the given argument wait ("fetch": its fetches) and
// its other commands answer; releaseOne lets one held command go. After
// failWith, a command of
// an armed forge prints the given diagnostic and fails at once instead, as
// Git does when its SSH transport prints a reason. The log names the forge of
// each held or failed command.
type silentForge struct {
	armed, only, failure, log string
	names                     []string
	release, releaseOne       func()
}

func holdForges(t *testing.T, cfg *gitcache.Config, binary string, forges map[string]string) *silentForge {
	t.Helper()
	dir := t.TempDir()
	s := &silentForge{armed: filepath.Join(dir, "armed"), only: filepath.Join(dir, "only"), failure: filepath.Join(dir, "failure"), log: filepath.Join(dir, "forge")}
	gate, script := filepath.Join(dir, "gate"), filepath.Join(dir, "git")
	if err := syscall.Mkfifo(gate, 0600); err != nil {
		t.Fatal(err)
	}
	var match strings.Builder
	for name, bare := range forges {
		fmt.Fprintf(&match, "  [ \"$a\" = '%s' ] && forge=%s\n", bare, name)
		s.names = append(s.names, name)
	}
	body := fmt.Sprintf("#!/bin/sh\nonly=$(cat '%[3]s' 2>/dev/null)\nforge= hit=\nfor a do\n  [ -n \"$only\" ] && [ \"$a\" = \"$only\" ] && hit=1\n%[1]sdone\n"+
		"if [ -n \"$forge\" ] && [ -e '%[2]s' ] && grep -qx \"$forge\" '%[2]s' && { [ -z \"$only\" ] || [ -n \"$hit\" ]; }; then\n"+
		"  echo \"$forge\" >> '%[4]s'\n  if [ -s '%[5]s' ]; then cat '%[5]s' >&2; exit 128; fi\n  read _ < '%[6]s'\nfi\nexec '%[7]s' \"$@\"\n",
		match.String(), s.armed, s.only, s.log, s.failure, gate, binary)
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Binary = script
	w, err := os.OpenFile(gate, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The writer stays open until cleanup, so a command that starts after a
	// release never waits for one to open the gate.
	t.Cleanup(func() { _ = w.Close() })
	s.release = func() { _, _ = w.WriteString(strings.Repeat("\n", 64)) }
	s.releaseOne = func() { _, _ = w.WriteString("\n") }
	return s
}

// arm holds the named forges, or all of them when none is named.
func (s *silentForge) arm(t *testing.T, names ...string) {
	t.Helper()
	if len(names) == 0 {
		names = s.names
	}
	if err := os.WriteFile(s.armed, []byte(strings.Join(names, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func (s *silentForge) holdOnly(t *testing.T, arg string) {
	t.Helper()
	if err := os.WriteFile(s.only, []byte(arg), 0600); err != nil {
		t.Fatal(err)
	}
}

func (s *silentForge) failWith(t *testing.T, diagnostic string) {
	t.Helper()
	if err := os.WriteFile(s.failure, []byte(diagnostic+"\nfatal: Could not read from remote repository.\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func (s *silentForge) asked(t *testing.T, forge string) int {
	t.Helper()
	data, err := os.ReadFile(s.log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Count(string(data), forge+"\n")
}

const (
	pinnedPipeline = "origin/team/pipeline"
	pinnedWire     = "origin/team/wire"
)

// pinFixture is a consumer (the fixture repository) whose pushes pin its own
// commit and two producers, as the own pipelines do, with every forge held
// silent once armed and the last refresh of each repository stale.
type pinFixture struct {
	// moved is another forge a repository's registration can move to.
	f, pipeline, wire, moved *gittest.Fixture
	remotes                  map[string]*gittest.Fixture
	cfg                      gitcache.Config
	forge                    *silentForge
	mu                       sync.Mutex
	now                      time.Time
	logs                     []string
	c                        *gitcache.Cache
}

func newPinFixture(t *testing.T) *pinFixture {
	t.Helper()
	x := &pinFixture{f: gittest.New(t), pipeline: gittest.New(t), wire: gittest.New(t), moved: gittest.New(t), now: time.Unix(2000000000, 0)}
	x.cfg = reviewConfig(t, x.f)
	x.cfg.Now = func() time.Time { x.mu.Lock(); defer x.mu.Unlock(); return x.now }
	x.cfg.Log = func(reason, detail string) {
		x.mu.Lock()
		defer x.mu.Unlock()
		x.logs = append(x.logs, reason+": "+detail)
	}
	x.remotes = map[string]*gittest.Fixture{gittest.Repository: x.f, pinnedPipeline: x.pipeline, pinnedWire: x.wire}
	x.cfg.Upstream = func(_ context.Context, repo string) (gitcache.Remote, error) {
		x.mu.Lock()
		f := x.remotes[repo]
		x.mu.Unlock()
		if f == nil {
			return gitcache.Remote{}, nil
		}
		return gitcache.Remote{URL: f.Bare, DefaultBranch: "refs/heads/main", RepositoryID: int64(len(repo))}, nil
	}
	x.forge = holdForges(t, &x.cfg, x.f.Binary, map[string]string{"consumer": x.f.Bare, "pipeline": x.pipeline.Bare, "wire": x.wire.Bare, "moved": x.moved.Bare})
	return x
}

func (x *pinFixture) start(t *testing.T) {
	t.Helper()
	x.c = reviewNew(t, x.cfg)
	t.Cleanup(x.forge.release)
	for _, repo := range []string{gittest.Repository, pinnedPipeline, pinnedWire} {
		if err := x.c.Init(t.Context(), repo); err != nil {
			t.Fatal(err)
		}
	}
	x.mu.Lock()
	x.now = x.now.Add(gitcache.RefreshInterval)
	x.mu.Unlock()
	x.forge.arm(t)
}

func (x *pinFixture) pins() []sourcecontract.Revision {
	return []sourcecontract.Revision{
		{Repository: gittest.Repository, Commit: x.f.Commit},
		{Repository: pinnedPipeline, Commit: x.pipeline.Commit},
		{Repository: pinnedWire, Commit: x.wire.Commit},
	}
}

// A push of a new branch whose pipeline pins its own commit and two producers
// is admitted within the real admission deadline while no forge answers: each
// producer pin waits its head wait and decides from the head the cache holds,
// and the own pin never asks the forge during its own admission.
func TestPinnedPushIsAdmittedWhileTheForgeDoesNotAnswer(t *testing.T) {
	x := newPinFixture(t)
	for i := range 100 {
		x.f.Git(t, x.f.Work, "-c", "user.name=t", "-c", "user.email=t@t", "tag", "-a", fmt.Sprintf("v0.%d.0", i), "-m", "release")
	}
	x.f.Git(t, x.f.Work, "push", "-q", x.f.Bare, "--tags")
	x.cfg.PreReceive = func(ctx context.Context, _ gitcache.ReceiveEvent, s *gitcache.ReceiveSource) error {
		for _, at := range x.pins() {
			if _, reachable, err := s.Pin(ctx, at); err != nil || !reachable {
				return fmt.Errorf("pin %s: reachable %v: %w", at.Repository, reachable, err)
			}
		}
		return nil
	}
	x.start(t)
	tip := x.f.Commit
	for i := range 45 {
		x.f.Write(t, "change", []byte(fmt.Sprint(i)))
		tip = x.f.CommitFile(t, fmt.Sprint("change ", i))
	}
	var diagnostics strings.Builder
	r := gateReceive(x.f)
	r.AlwaysRun = true
	r.Input = receiveRefInput(t, x.f, gitcache.RefUpdate{Ref: "refs/heads/land/oberth-v030", NewSHA: tip})
	r.Output, r.Diagnostics = io.Discard, &diagnostics
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil }
	if err := x.c.Receive(t.Context(), r); err != nil {
		x.mu.Lock()
		defer x.mu.Unlock()
		t.Fatalf("the pinned push was refused while the forge did not answer: %v\n%s\n%s", err, diagnostics.String(), strings.Join(x.logs, "\n"))
	}
	x.mu.Lock()
	logs := strings.Join(x.logs, "\n")
	x.mu.Unlock()
	if strings.Contains(logs, "signal: killed") {
		t.Fatal("a forge command was killed at a caller's deadline", logs)
	}
	if got := x.forge.asked(t, "consumer"); got != 0 {
		t.Fatal("the own pin asked the forge during its own admission", got)
	}
	if x.forge.asked(t, "pipeline") == 0 || x.forge.asked(t, "wire") == 0 {
		t.Fatal("the producer pins did not ask the forge")
	}
}

// A rerun's admission (no receive, the 10-second request bound) pins the same
// producers through the cache directly and decides within its bound, also
// with a shorter head wait.
func TestRerunPinDecidesFromTheCacheWhileTheForgeDoesNotAnswer(t *testing.T) {
	x := newPinFixture(t)
	x.start(t)
	gitcache.SetHeadWait(x.c, 500*time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for _, at := range x.pins()[1:] {
		if id, reachable, err := x.c.Pin(ctx, at); err != nil || !reachable || id == 0 {
			t.Fatal("a rerun pin was not decided from the cache", at.Repository, id, reachable, err)
		}
	}
	if ctx.Err() != nil {
		t.Fatal("the rerun pins used the whole request bound")
	}
}
