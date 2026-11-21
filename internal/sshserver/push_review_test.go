package sshserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestMain(m *testing.M) {
	if code, handled := baseSSHInvocation(); handled {
		os.Exit(code)
	}
	if sock := os.Getenv("REVIEW_PROXY_SOCK"); sock != "" {
		os.Exit(reviewProxy(sock))
	}
	if code, handled := gitcache.ReceiveHookInvocation(context.Background(), os.Args, os.Stdin, os.Stderr); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func reviewProxy(sock string) int {
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return 2
	}
	go func() { _, _ = io.Copy(conn, os.Stdin); _ = conn.(*net.UnixConn).CloseWrite() }()
	_, _ = io.Copy(os.Stdout, conn)
	return 0
}

type reviewEnv struct {
	f     *gittest.Fixture
	c     *gitcache.Cache
	gate  func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error
	abort []string
}

func newReviewEnv(t *testing.T) *reviewEnv {
	t.Helper()
	return newReviewEnvConfigured(t, nil)
}
func newReviewEnvConfigured(t *testing.T, configure func(*gitcache.Config)) *reviewEnv {
	t.Helper()
	f := gittest.New(t)
	f.Git(t, f.Work, "tag", "-a", "v1", "-m", "release v1")
	f.Git(t, f.Work, "push", f.Bare, "refs/tags/v1")
	e := &reviewEnv{f: f}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	config := gitcache.Config{Root: filepath.Join(f.Root, "cache"), Scratch: filepath.Join(f.Root, "scratch"), Binary: f.Binary, HookBinary: reviewHook(t, bin),
		ReservedBranches: []string{"release/*"},
		LiveOwners:       func(context.Context, string) (gitcache.OwnerPage, error) { return gitcache.OwnerPage{}, nil },
		PreReceive: func(ctx context.Context, ev gitcache.ReceiveEvent, s *gitcache.ReceiveSource) error {
			if e.gate == nil {
				return nil
			}
			return e.gate(ctx, ev, s)
		},
		AbortReceive: func(id string) { e.abort = append(e.abort, id) }}
	if configure != nil {
		configure(&config)
	}
	c, err := gitcache.New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err = c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	f.Git(t, f.Store(), "fetch", f.Bare, "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*")
	e.c = c
	return e
}

type pushResult struct {
	client, ssh string
	err         error
}

func (e *reviewEnv) push(t *testing.T, r gitcache.ReceiveRequest, args ...string) pushResult {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ssh-push-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	sock := filepath.Join(dir, "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	var diag bytes.Buffer
	done := make(chan error, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			done <- err
			return
		}
		r.Input, r.Output, r.Diagnostics = conn, conn, &diag
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		err = e.c.Receive(ctx, r)
		_ = conn.Close()
		done <- err
	}()
	bin, _ := os.Executable()
	cmd := exec.Command(e.f.Binary, append([]string{"push", "--receive-pack=" + bin}, args...)...)
	cmd.Dir = e.f.Work
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LANG=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GORACE=atexit_sleep_ms=0", "REVIEW_PROXY_SOCK=" + sock}
	out, _ := cmd.CombinedOutput()
	var res pushResult
	select {
	case res.err = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("receive did not finish")
	}
	res.client = string(out)
	res.ssh = diag.String()
	if res.err != nil {
		if text := gitFailure(res.err); text != "" {
			res.ssh += text + "\n"
		}
	}
	return res
}

func agent(f *gittest.Fixture) gitcache.ReceiveRequest {
	return gitcache.ReceiveRequest{Repository: gittest.Repository, Actor: "agent", Fingerprint: "key", Admission: gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: f.Commit},
		Deliver: func(_ context.Context, ev gitcache.ReceiveEvent) ([]string, error) {
			return []string{fmt.Sprintf("delivered %d updates run=%v", len(ev.Updates), ev.Options.Run), "  watch: https://oberth.local/runs/r1"}, nil
		}}
}

func (e *reviewEnv) ref(t *testing.T, ref string) string {
	cmd := exec.Command(e.f.Binary, "for-each-ref", "--format=%(objectname)", ref)
	cmd.Dir = e.f.Store()
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

func show(t *testing.T, name string, r pushResult) {
	t.Helper()
	combined := r.client + r.ssh
	for _, reason := range []string{"tags cannot move or be deleted", "branch deletion is protected", "default branch requires promotion", "invalid Git push options", "step build: image registry is not allowed", "template registry is unreachable", "push admission is unavailable"} {
		if n := strings.Count(combined, reason); n > 1 {
			t.Fatalf("refusal printed %d times: %s", n, combined)
		}
	}
	if name == "retry after save failure" && r.ssh != "" {
		t.Fatal("earlier receive feedback reached a new transport", combined)
	}
	if name == "save fails" && !strings.Contains(r.ssh, "push saved") {
		t.Fatal("accepted push not acknowledged", combined)
	}
	t.Logf("==== %s ====\n--- client (git push output) ---\n%s--- ssh stderr (delivery lines + server error) ---\n%s--- err: %v", name, r.client, r.ssh, r.err)
}

const remote = "/review/origin/team/widget.git"

func TestReviewPushMessages(t *testing.T) {
	t.Run("parked", func(t *testing.T) {
		e := newReviewEnv(t)
		e.f.Write(t, "a.txt", []byte("a"))
		e.f.CommitFile(t, "a")
		res := e.push(t, agent(e.f), remote, "HEAD:refs/heads/feature")
		show(t, "parked agent push", res)
	})
	t.Run("started", func(t *testing.T) {
		e := newReviewEnv(t)
		e.f.Write(t, "a.txt", []byte("a"))
		e.f.CommitFile(t, "a")
		res := e.push(t, agent(e.f), "-o", "oberth.run=1", remote, "HEAD:refs/heads/feature")
		show(t, "run=1 accepted", res)
	})
	t.Run("gate refused", func(t *testing.T) {
		e := newReviewEnv(t)
		e.gate = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error {
			pe := publicerr.New(publicerr.InvalidInput, "step build: image registry is not allowed", "Use an image from an approved registry.", nil)
			pe.Detail = ".oberth/build.yaml: templates[0].container.image"
			return pe
		}
		e.f.Write(t, "a.txt", []byte("a"))
		e.f.CommitFile(t, "a")
		res := e.push(t, agent(e.f), "-o", "oberth.run=1", remote, "HEAD:refs/heads/feature")
		show(t, "gate refused", res)
		if e.ref(t, "refs/heads/feature") != "" {
			t.Error("refused push moved ref")
		}
	})
	t.Run("gate unavailable", func(t *testing.T) {
		e := newReviewEnv(t)
		e.gate = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error {
			return publicerr.New(publicerr.Unavailable, "template registry is unreachable", "Retry the push in a minute.", nil)
		}
		e.f.Write(t, "a.txt", []byte("a"))
		e.f.CommitFile(t, "a")
		res := e.push(t, agent(e.f), "-o", "oberth.run=1", remote, "HEAD:refs/heads/feature")
		show(t, "gate unavailable", res)
	})
	t.Run("agent default", func(t *testing.T) {
		e := newReviewEnv(t)
		e.f.Write(t, "a.txt", []byte("a"))
		e.f.CommitFile(t, "a")
		res := e.push(t, agent(e.f), remote, "HEAD:refs/heads/main")
		show(t, "agent pushes main", res)
	})
	t.Run("tag move", func(t *testing.T) {
		e := newReviewEnv(t)
		e.f.Write(t, "a.txt", []byte("a"))
		e.f.CommitFile(t, "a")
		e.f.Git(t, e.f.Work, "tag", "-f", "v1")
		res := e.push(t, agent(e.f), "--force", remote, "refs/tags/v1")
		show(t, "forced tag move", res)
	})
	t.Run("tag delete", func(t *testing.T) {
		e := newReviewEnv(t)
		res := e.push(t, agent(e.f), remote, ":refs/tags/v1")
		show(t, "tag delete", res)
	})
	t.Run("operator deletes main", func(t *testing.T) {
		e := newReviewEnv(t)
		r := agent(e.f)
		r.AllowDefault = true
		res := e.push(t, r, remote, ":refs/heads/main")
		show(t, "operator deletes main", res)
	})
	t.Run("reserved delete", func(t *testing.T) {
		e := newReviewEnv(t)
		e.f.Git(t, e.f.Store(), "update-ref", "refs/heads/release/1", e.f.Commit)
		res := e.push(t, agent(e.f), remote, ":refs/heads/release/1")
		show(t, "reserved branch delete", res)
	})
	t.Run("bad option", func(t *testing.T) {
		e := newReviewEnv(t)
		e.f.Write(t, "a.txt", []byte("a"))
		e.f.CommitFile(t, "a")
		res := e.push(t, agent(e.f), "-o", "oberth.priority=high", remote, "HEAD:refs/heads/feature")
		show(t, "unknown push option", res)
	})
	t.Run("gate not configured", func(t *testing.T) {
		e := newReviewEnv(t)
		e.f.Write(t, "a.txt", []byte("a"))
		e.f.CommitFile(t, "a")
		r := agent(e.f)
		r.AlwaysRun = true
		e.c = nil
		e2 := newReviewEnvNoGate(t, e.f)
		res := e2.push(t, r, remote, "HEAD:refs/heads/feature")
		show(t, "operator push, gate not configured", res)
	})
	t.Run("multi ref one bad", func(t *testing.T) {
		e := newReviewEnv(t)
		e.f.Write(t, "a.txt", []byte("a"))
		e.f.CommitFile(t, "a")
		e.f.Git(t, e.f.Work, "tag", "-f", "v1")
		res := e.push(t, agent(e.f), "--force", remote, "HEAD:refs/heads/feature2", "refs/tags/v1")
		show(t, "multi-ref with tag move", res)
		if e.ref(t, "refs/heads/feature2") != "" {
			t.Error("feature2 created although push refused")
		}
	})
	t.Run("force feature", func(t *testing.T) {
		e := newReviewEnv(t)
		e.f.Git(t, e.f.Store(), "update-ref", "refs/heads/feature", e.f.Commit)
		e.f.Git(t, e.f.Work, "checkout", "--orphan", "other")
		e.f.Write(t, "b.txt", []byte("b"))
		e.f.CommitFile(t, "b")
		res := e.push(t, agent(e.f), "--force", remote, "HEAD:refs/heads/feature")
		show(t, "forced non-ff feature", res)
	})
	t.Run("save fails then retry", func(t *testing.T) {
		e := newReviewEnv(t)
		e.f.Write(t, "a.txt", []byte("a"))
		e.f.CommitFile(t, "a")
		r := agent(e.f)
		ok := r.Deliver
		r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
			return nil, errors.New("database is locked")
		}
		res := e.push(t, r, "-o", "oberth.run=1", remote, "HEAD:refs/heads/feature")
		show(t, "save fails", res)
		calls := 0
		r.Deliver = func(ctx context.Context, event gitcache.ReceiveEvent) ([]string, error) {
			calls++
			return ok(ctx, event)
		}
		res = e.push(t, r, "-o", "oberth.run=1", remote, "HEAD:refs/heads/feature")
		show(t, "retry after save failure", res)
		if res.err != nil || calls != 1 {
			t.Fatal("saved receive was not replayed once", res.err, calls)
		}
		if _, err := e.c.PendingReceive(t.Context(), gittest.Repository); !os.IsNotExist(err) {
			t.Fatal("saved receive was not acknowledged", err)
		}
	})
}

func newReviewEnvNoGate(t *testing.T, f *gittest.Fixture) *reviewEnv {
	bin, _ := os.Executable()
	c, err := gitcache.New(t.Context(), gitcache.Config{Root: filepath.Join(f.Root, "cache2"), Scratch: filepath.Join(f.Root, "scratch2"), Binary: f.Binary, HookBinary: reviewHook(t, bin),
		LiveOwners: func(context.Context, string) (gitcache.OwnerPage, error) { return gitcache.OwnerPage{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err = c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	f.Git(t, filepath.Join(f.Root, "cache2", "origin", "team", "widget.git"), "fetch", f.Bare, "+refs/heads/*:refs/heads/*")
	return &reviewEnv{f: f, c: c}
}

func reviewHook(t *testing.T, binary string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hook")
	body := "#!/usr/bin/python3\nimport os,sys\nos.environ['GORACE']='atexit_sleep_ms=0'\nos.execve(" + strconv.Quote(binary) + ",sys.argv,os.environ)\n"
	if err := os.WriteFile(path, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
