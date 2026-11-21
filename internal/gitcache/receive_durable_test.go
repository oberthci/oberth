package gitcache_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

type preparationStop struct {
	Root, Scratch, Binary, Hook, Store, Pack, Commit string
	Boundary, Damage                                 string
}

func TestReceivePreparationSurvivesProcessStop(t *testing.T) {
	for _, damage := range []string{"intact", "bypassed"} {
		for _, boundary := range []string{"before", "after", "committed"} {
			t.Run(damage+"/"+boundary, func(t *testing.T) { checkPreparationStop(t, damage, boundary) })
		}
	}
}

func checkPreparationStop(t *testing.T, damage, boundary string) {
	t.Helper()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	c := thirdInit(t, cfg)
	f.Git(t, f.Store(), "update-ref", "refs/heads/topic", f.Commit)
	f.Write(t, "stop.txt", []byte("prepared source"))
	next := f.CommitFile(t, "stop")
	input := receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/topic", OldSHA: f.Commit, NewSHA: next})
	defer func() { _ = input.Close() }()
	pack, err := io.ReadAll(input)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.Root, "receive.pack")
	if err = os.WriteFile(path, pack, 0600); err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	shape := preparationStop{cfg.Root, cfg.Scratch, cfg.Binary, cfg.HookBinary, f.Store(), path, f.Commit, boundary, damage}
	runPreparationStop(t, shape)
	record := readCompletionRecord(t, f)
	if record.State != "ready" || (record.Prepared != nil) != (damage == "intact" && boundary != "before") {
		t.Fatal("wrong durable fact at process stop", record.State, record.Prepared)
	}
	c = reopenStoppedReceive(t, cfg)
	if boundary == "after" {
		assertEmptyPreparationSettlesOnRead(t, c, f)
	}
	delivered := 0
	deliver := func(context.Context, gitcache.ReceiveEvent) ([]string, error) { delivered++; return nil, nil }
	assertCompletedPush(t, c, f, completionExpectation{
		next: next, accepted: damage == "intact" && boundary == "committed", delivered: &delivered, deliver: deliver,
	})
	if completionState(t, f) != "absent" {
		t.Fatal("recovery retained a finished receive")
	}
}

func reopenStoppedReceive(t *testing.T, cfg gitcache.Config) *gitcache.Cache {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		c, err := gitcache.New(ctx, cfg)
		if err == nil {
			t.Cleanup(func() { _ = c.Close() })
			return c
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("stopped receive still owns its cache", err)
		case <-tick.C:
		}
	}
}

func runPreparationStop(t *testing.T, shape preparationStop) {
	t.Helper()
	data, err := json.Marshal(shape)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	worker := exec.CommandContext(ctx, binary, "-test.run=^TestReceivePreparationStopWorker$")
	worker.Env = append(os.Environ(), "OBERTH_PREPARATION_STOP="+string(data))
	output, err := worker.CombinedOutput()
	var exit *exec.ExitError
	if ctx.Err() != nil || !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatal("worker did not stop at the native boundary", err, string(output))
	}
}

func TestReceivePreparationStopWorker(t *testing.T) {
	data := os.Getenv("OBERTH_PREPARATION_STOP")
	if data == "" {
		return
	}
	var shape preparationStop
	if err := json.Unmarshal([]byte(data), &shape); err != nil {
		t.Fatal(err)
	}
	c := reviewNew(t, gitcache.Config{Root: shape.Root, Scratch: shape.Scratch, Binary: shape.Binary, HookBinary: shape.Hook,
		PreReceive: func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { return nil }})
	input, err := os.Open(shape.Pack)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close() }()
	var once sync.Once
	r := gitcache.ReceiveRequest{Repository: gittest.Repository, Actor: "agent", Fingerprint: "key", AlwaysRun: true,
		Admission: gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: shape.Commit},
		Input:     input, Output: io.Discard, Diagnostics: io.Discard}
	r.Begin = func() { once.Do(func() { installPreparationStop(t, shape, os.Getpid()) }) }
	if shape.Boundary == "transport" {
		status := &completionStatus{}
		r.Output = status
		gitcache.AfterReceiveTransport(c, func() {
			if status.seen.Load() {
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			}
		})
	}

	t.Fatal("worker survived stop", c.Receive(t.Context(), r))
}

func installPreparationStop(t *testing.T, shape preparationStop, server int) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(shape.Store, "hooks", "reference-transaction")
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	body := "#!/usr/bin/python3\nimport os,sys,subprocess,signal\n" +
		"os.environ['GORACE']='atexit_sleep_ms=0'\nphase=sys.argv[1]\n" +
		"boundary=" + strconv.Quote(shape.Boundary) + "\n" +
		"def stop():\n os.kill(" + strconv.Itoa(server) + " or os.getppid(),signal.SIGKILL)\n os._exit(79)\n" +
		"if phase=='prepared' and boundary=='before': stop()\n" +
		"if " + strconv.Quote(shape.Damage) + " in ('intact','between','empty'):\n" +
		" data={'input':b''} if " + strconv.Quote(shape.Damage) + "=='empty' else {}\n" +
		" result=subprocess.run(['reference-transaction',phase],executable=" + strconv.Quote(binary) + ",**data)\n" +
		" if result.returncode: sys.exit(result.returncode)\n" +
		"if phase=='committed' and " + strconv.Quote(shape.Damage) + "=='between': os.unlink(" + strconv.Quote(path) + ")\n" +
		"if (phase=='prepared' and boundary=='after') or (phase=='committed' and boundary=='committed'): stop()\n"
	if err = os.WriteFile(path, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
}

func readCompletionRecord(t *testing.T, f *gittest.Fixture) gitcache.Reservation {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.Store(), "oberth-receive.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record gitcache.Reservation
	if err = json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestKilledGitAfterPreparationRejectsEmptyCommit(t *testing.T) {
	f, c, next := completionFixture(t)
	r := completionRequest(t, f, next)
	var once sync.Once
	r.Begin = func() {
		once.Do(func() {
			installPreparationStop(t, preparationStop{Store: f.Store(), Boundary: "after", Damage: "intact"}, 0)
		})
	}
	delivered := 0
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { delivered++; return nil, nil }
	if err := c.Receive(t.Context(), r); err == nil {
		t.Fatal("killed native transport returned success")
	}
	if completionState(t, f) != "absent" {
		t.Fatal("prepared but uncommitted receive retained a record")
	}
	assertCompletedPush(t, c, f, completionExpectation{next: next, delivered: &delivered, deliver: r.Deliver})
}

func assertEmptyPreparationSettlesOnRead(t *testing.T, c *gitcache.Cache, f *gittest.Fixture) {
	t.Helper()
	ref, err := c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/topic")
	if err != nil || ref.ObjectSHA != f.Commit || completionState(t, f) != "absent" {
		t.Fatal("ordinary read retained a prepared receive with nothing committed", ref, err)
	}
}
