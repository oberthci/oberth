package gitcache_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
)

type rollbackFixture struct {
	f                   *gittest.Fixture
	c                   *gitcache.Cache
	config              gitcache.Config
	accepted, unchecked string
}

func failedRollback(t *testing.T, configure ...func(*gitcache.Config)) rollbackFixture {
	t.Helper()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = failedRollbackWrapper(t, f, cfg)
	var err error
	cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { return nil }
	for _, apply := range configure {
		apply(&cfg)
	}
	c := thirdInit(t, cfg)
	f.Write(t, "file.txt", []byte("accepted"))
	accepted := f.CommitFile(t, "accepted")
	if err = c.Receive(t.Context(), fourthAgentPush(t, f,
		[]gitcache.RefUpdate{{Ref: "refs/heads/topic", NewSHA: accepted}}, nil)); err != nil {
		t.Fatal(err)
	}
	f.Git(t, f.Work, "checkout", "--orphan", "unchecked")
	f.Write(t, "file.txt", []byte("unchecked"))
	unchecked := f.CommitFile(t, "unchecked")
	for _, name := range []string{"swap", "arm"} {
		if err = os.WriteFile(filepath.Join(f.Root, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	err = c.Receive(t.Context(), fourthAgentPush(t, f,
		[]gitcache.RefUpdate{{Ref: "refs/heads/topic", OldSHA: accepted, NewSHA: unchecked}}, nil))
	if err == nil || !strings.Contains(err.Error(), "refs/heads/topic") {
		t.Fatal("rollback failure unnamed", err)
	}
	if _, err = os.Stat(filepath.Join(f.Store(), "oberth-receive.json")); err != nil {
		t.Fatal("rollback forgotten", err)
	}
	return rollbackFixture{f, c, cfg, accepted, unchecked}
}

func TestFailedRollbackBlocksReceivesAndSourceUntilRetry(t *testing.T) {
	t.Parallel()
	r := failedRollback(t)
	_, err := r.c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/topic")
	assertRollbackBlocked(t, err)
	_, err = r.c.Stage(t.Context(), pinBinding(r.f, r.unchecked))
	assertRollbackBlocked(t, err)
	called := false
	err = r.c.RetryReceive(t.Context(), gittest.Repository,
		func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
			called = true
			return nil, nil
		})
	assertRollbackBlocked(t, err)
	err = r.c.Receive(t.Context(), fourthAgentPush(t, r.f,
		[]gitcache.RefUpdate{{Ref: "refs/heads/later", NewSHA: r.f.Commit}}, nil))
	assertRollbackBlocked(t, err)
	if called {
		t.Fatal("unchecked receive delivered")
	}
	assertRollbackBlocked(t, r.c.GC(t.Context()))
	if exec128(r.f, "cat-file", "-e", r.accepted) {
		t.Fatal("maintenance pruned predecessor")
	}
	if err = os.Remove(filepath.Join(r.f.Root, "arm")); err != nil {
		t.Fatal(err)
	}
	ref, err := r.c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/topic")
	if err != nil || ref.ObjectSHA != r.accepted {
		t.Fatal("next read did not retry rollback", ref, err)
	}
	if err = r.c.ReplayPending(t.Context(), func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
		called = true
		return nil, nil
	}); err != nil || called {
		t.Fatal("rollback replayed as a delivery", err, called)
	}
	if _, err = os.Stat(filepath.Join(r.f.Store(), "oberth-receive.json")); !os.IsNotExist(err) {
		t.Fatal("restored receive not settled", err)
	}
}

func TestOpenRetriesRollbackBeforeServing(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	r := failedRollback(t)
	if err := r.c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(r.f.Root, "arm")); err != nil {
		t.Fatal(err)
	}
	c := reviewNew(t, r.config)
	if got := r.f.Git(t, r.f.Store(), "rev-parse", "refs/heads/topic"); got != r.accepted {
		t.Fatal("open did not retry", got)
	}
	ref, err := c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/topic")
	if err != nil || ref.ObjectSHA != r.accepted {
		t.Fatal(ref, err)
	}
}

func assertRollbackBlocked(t *testing.T, err error) {
	t.Helper()
	var unavailable *gitport.UnavailableError
	if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "refs/heads/topic") {
		t.Fatal("rollback failure lost ref/class", err)
	}
}

func TestRollbackRecordWriteFailureKeepsNamedRecovery(t *testing.T) {
	t.Parallel()
	r := failedRollback(t)
	path := filepath.Join(r.f.Store(), "oberth-receive.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record gitcache.Reservation
	if err = json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	record.State = "reserved"
	data, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(r.f.Store(), 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(r.f.Store(), 0700) })
	_, err = r.c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/topic")
	assertRollbackBlocked(t, err)
	if _, err = os.Stat(path); err != nil {
		t.Fatal("write failure forgot recovery", err)
	}
	if err = os.Chmod(r.f.Store(), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(r.f.Root, "arm")); err != nil {
		t.Fatal(err)
	}
	ref, err := r.c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/topic")
	if err != nil || ref.ObjectSHA != r.accepted {
		t.Fatal("storage recovery did not restore predecessor", ref, err)
	}
}

func failedRollbackWrapper(t *testing.T, f *gittest.Fixture, cfg gitcache.Config) string {
	t.Helper()
	original, err := os.ReadFile(cfg.HookBinary)
	if err != nil {
		t.Fatal(err)
	}
	saved := filepath.Join(f.Root, "hook.saved")
	if err = os.WriteFile(saved, original, 0700); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(f.Root, "git")
	script := "#!/bin/sh\nR=" + shellQuote(f.Root) + "\ncase \" $* \" in\n" +
		"*' receive-pack '*) if [ -e \"$R/swap\" ]; then /bin/rm -- \"$R/swap\" " + shellQuote(cfg.HookBinary) +
		"; /usr/bin/git \"$@\"; rc=$?; /bin/cp -p \"$R/hook.saved\" " + shellQuote(cfg.HookBinary) + "; exit $rc; fi;;\n" +
		"*' update-ref '*' --no-deref '*) if [ -e \"$R/arm\" ]; then kill -KILL $$; fi;;\n" +
		"esac\nexec /usr/bin/git \"$@\"\n"
	if err = os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return wrapper
}
