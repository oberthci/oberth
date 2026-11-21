package gitcache_test

import (
	"context"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func fourthAgentPush(
	t *testing.T, f *gittest.Fixture, updates []gitcache.RefUpdate, options []string,
) gitcache.ReceiveRequest {
	return gitcache.ReceiveRequest{
		Repository: gittest.Repository, Actor: "agent", Fingerprint: "key", AlwaysRun: true,
		Admission: gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: f.Commit},
		Input:     receiveBatchInput(t, f, updates, options), Output: io.Discard,
		Deliver: func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil }}
}
func fourthCrashFixture(t *testing.T) (*gittest.Fixture, *gitcache.Cache, gitcache.Config, string) {
	t.Helper()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	_ = reviewWrapper(t, f)
	root := f.Root
	original, err := os.ReadFile(cfg.HookBinary)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "hook.saved"), original, 0700); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(root, "crash-git")
	body := "#!/bin/sh\nR=" + shellQuote(root) + "\ncase \" $* \" in\n" +
		"*' receive-pack '*) if [ -e \"$R/swap\" ]; then /bin/rm -- \"$R/swap\"; " +
		"/bin/rm -- " + shellQuote(cfg.HookBinary) + "; fi;;\n" +
		"*' update-ref '*' --no-deref '*) if [ -e \"$R/arm\" ]; then /bin/rm -- \"$R/arm\"; " +
		"/bin/cp -p \"$R/hook.saved\" " + shellQuote(cfg.HookBinary) +
		"; kill -KILL $$; fi;;\n" +
		"esac\nexec /usr/bin/git \"$@\"\n"
	if err = os.WriteFile(wrapper, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Binary = wrapper
	cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { return nil }
	c := thirdInit(t, cfg)
	f.Write(t, "file.txt", []byte("accepted work\n"))
	accepted := f.CommitFile(t, "accepted")
	if err = c.Receive(t.Context(), fourthAgentPush(t, f,
		[]gitcache.RefUpdate{{Ref: "refs/heads/topic", NewSHA: accepted}}, nil)); err != nil {
		t.Fatal("accepted push:", err)
	}
	return f, c, cfg, accepted
}

func TestFourthRollbackAfterCrashAndMaintenance(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	f, c, cfg, accepted := fourthCrashFixture(t)
	root := f.Root
	var err error
	f.Git(t, f.Work, "checkout", "-q", "--orphan", "other")
	f.Write(t, "file.txt", []byte("unreviewed replacement\n"))
	bad := f.CommitFile(t, "unreviewed")
	for _, name := range []string{"swap", "arm"} {
		if err = os.WriteFile(filepath.Join(root, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	firstErr := c.Receive(t.Context(), fourthAgentPush(t, f,
		[]gitcache.RefUpdate{{Ref: "refs/heads/topic", OldSHA: accepted, NewSHA: bad}}, nil))
	t.Logf("unchecked force push with its rollback killed: %v", firstErr)
	t.Logf("topic after the killed rollback: unchecked=%v", f.Git(t, f.Store(), "rev-parse", "refs/heads/topic") == bad)
	_, recordErr := os.Stat(filepath.Join(f.Store(), "oberth-receive.json"))
	t.Logf("receive record kept for a retry of the rollback: %v", recordErr == nil)
	_ = c.Close()
	reopened, err := gitcache.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	gcErr := reopened.GC(t.Context())
	missing := exec128(f, "cat-file", "-e", accepted)
	t.Logf("maintenance before the next receive: %v; accepted predecessor pruned=%v", gcErr, missing)
	nextErr := reopened.Receive(t.Context(), fourthAgentPush(t, f,
		[]gitcache.RefUpdate{{Ref: "refs/heads/later", NewSHA: f.Commit}}, nil))
	topic := f.Git(t, f.Store(), "for-each-ref", "--format=%(objectname)", "refs/heads/topic")
	t.Logf("next push: %v", nextErr)
	t.Logf("topic now: accepted=%v unchecked=%v absent=%v", topic == accepted, topic == bad, topic == "")
	if topic == bad {
		t.Fatal("the unchecked commit is still the public head after recovery was attempted")
	}
	if nextErr != nil {
		t.Fatal("the repository refuses pushes after the interrupted rollback")
	}
}
func exec128(f *gittest.Fixture, args ...string) bool {
	cmd := exec.Command(f.Binary, args...)
	cmd.Dir = f.Store()
	return cmd.Run() != nil
}

// After the local branch that blocked an upstream name is gone, the upstream
// branch should become visible in the cache.
