package gitcache_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestUnpreparedUpdateRecoversBetweenRestorePhases(t *testing.T) {
	for _, shape := range []string{"update", "shape-and-update"} {
		for _, mode := range []string{"bypassed-stop", "empty-live"} {
			t.Run(shape+"/"+mode, func(t *testing.T) { checkRestorePhaseStop(t, shape, mode) })
		}
	}
}

func checkRestorePhaseStop(t *testing.T, shape, mode string) {
	t.Helper()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { return nil }
	c := thirdInit(t, cfg)
	updates := restoreStopUpdates(t, f, shape)
	before := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname) %(objectname)")
	r := fourthAgentPush(t, f, updates, nil)
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
		t.Error("unprepared update delivered")
		return nil, nil
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if mode == "bypassed-stop" {
		stopUnpreparedRefShape(t, preparationStop{Root: cfg.Root, Scratch: cfg.Scratch, Binary: cfg.Binary,
			Hook: cfg.HookBinary, Store: f.Store(), Commit: f.Commit, Boundary: "transport", Damage: "bypassed"}, r.Input)
	}
	cfg.Binary = stopSecondRestoreTransaction(t, f)
	c = reopenStoppedReceive(t, cfg)
	if mode == "empty-live" {
		var once sync.Once
		r.Begin = func() {
			once.Do(func() { installPreparationStop(t, preparationStop{Store: f.Store(), Damage: "empty"}, 0) })
		}
		if err := c.Receive(t.Context(), r); err == nil {
			t.Fatal("interrupted live restore returned success")
		}
	}
	assertStoppedRestore(t, f)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Binary = f.Binary
	c = reopenStoppedReceive(t, cfg)
	assertResumedRestore(t, c, f, before)
}

func restoreStopUpdates(t *testing.T, f *gittest.Fixture, shape string) []gitcache.RefUpdate {
	t.Helper()
	f.Git(t, f.Store(), "update-ref", "refs/heads/topic", f.Commit)
	f.Write(t, "restore-stop.txt", []byte("updated source"))
	next := f.CommitFile(t, "restore stop")
	if shape == "update" {
		return []gitcache.RefUpdate{{Ref: "refs/heads/topic", OldSHA: f.Commit, NewSHA: next}}
	}
	f.Git(t, f.Store(), "update-ref", "refs/heads/other", f.Commit)
	return []gitcache.RefUpdate{{Ref: "refs/heads/topic", OldSHA: f.Commit},
		{Ref: "refs/heads/topic/leaf", NewSHA: next}, {Ref: "refs/heads/other", OldSHA: f.Commit, NewSHA: next}}
}

func stopSecondRestoreTransaction(t *testing.T, f *gittest.Fixture) string {
	t.Helper()
	path := filepath.Join(f.Root, "stop-restore-git")
	body := `#!/usr/bin/python3
import os,sys,signal
counter=os.path.join(os.path.dirname(__file__),'restore-transactions')
if sys.argv[-3:]==['update-ref','--stdin','--no-deref']:
 try:
  with open(counter) as stream: count=int(stream.read())
 except FileNotFoundError:
  count=0
 with open(counter,'w') as stream: stream.write(str(count+1))
 if count==1: os.kill(os.getpid(),signal.SIGKILL)
os.execv(` + strconv.Quote(f.Binary) + `,['git']+sys.argv[1:])
`
	if err := os.WriteFile(path, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertStoppedRestore(t *testing.T, f *gittest.Fixture) {
	t.Helper()
	count, err := os.ReadFile(filepath.Join(f.Root, "restore-transactions"))
	if err != nil || string(count) != "2" {
		t.Fatal("restore did not stop at its second transaction", string(count), err)
	}
	for _, ref := range []string{"refs/heads/topic", "refs/heads/other"} {
		if got := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", ref); got != "" {
			t.Fatal("delete phase did not commit before stop", got)
		}
	}
	if state := completionState(t, f); state != "rejected" {
		t.Error("restore began before durable rejection", state)
	}
}

func assertResumedRestore(t *testing.T, c *gitcache.Cache, f *gittest.Fixture, before string) {
	t.Helper()
	retryCompletion(t, c, func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
		t.Error("restarted unprepared update delivered")
		return nil, nil
	})
	if got := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname) %(objectname)"); got != before {
		t.Fatal("restart did not restore the complete ref inventory", got, before)
	}
	ref, err := c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/topic")
	if err != nil || ref.CommitSHA != f.Commit || completionState(t, f) != "absent" {
		t.Fatal("restart left the repository fenced", ref, err)
	}
	r := fourthAgentPush(t, f, []gitcache.RefUpdate{{Ref: "refs/heads/after-restore", NewSHA: f.Commit}}, nil)
	calls := 0
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { calls++; return nil, nil }
	if err = c.Receive(t.Context(), r); err != nil || calls != 1 {
		t.Fatal("ordinary push failed after restarted restore", err, calls)
	}
}
