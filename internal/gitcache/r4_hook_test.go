package gitcache_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
)

func TestReceiveRejectsChangedHookContent(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{"empty": "", "replacement": "#!/bin/sh\nexit 0\n"} {
		t.Run(name, func(t *testing.T) {
			f := gittest.New(t)
			cfg := reviewConfig(t, f)
			c := thirdInit(t, cfg)
			if err := os.WriteFile(cfg.HookBinary, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			err := assertUncheckedRefused(t, c, f)
			var unavailable *gitport.UnavailableError
			if !errors.As(err, &unavailable) {
				t.Fatal("changed hook reached native receive", err)
			}
		})
	}
}
func TestReceiveRollsBackWhenHookRemovedAfterCheck(t *testing.T) {
	t.Parallel()
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing", true: "empty"}[empty], func(t *testing.T) { checkRemovedHookRace(t, empty) })
	}
}
func checkRemovedHookRace(t *testing.T, empty bool) {
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	wrapper := filepath.Join(f.Root, "remove-hook-git")
	body := "#!/bin/sh\ncase \" $* \" in\n*' receive-pack '*) /bin/rm -- " + shellQuote(cfg.HookBinary) +
		";;\nesac\nexec /usr/bin/git \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Binary = wrapper
	if empty {
		cfg.Upstream = nil
	}
	c := thirdInit(t, cfg)
	if !empty {
		_ = assertUncheckedRefused(t, c, f)
		return
	}
	err := c.Receive(t.Context(), gitcache.ReceiveRequest{
		Repository: gittest.Repository, Actor: "agent", Fingerprint: "key",
		AlwaysRun: true, Admission: gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main"},
		Input: receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/feature", NewSHA: f.Commit}), Output: io.Discard})
	if err == nil {
		t.Fatal("first unchecked push accepted")
	}
	if got := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/heads/"); got != "" {
		t.Fatal("first unchecked ref survived", got)
	}
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
func assertUncheckedRefused(t *testing.T, c *gitcache.Cache, f *gittest.Fixture) error {
	t.Helper()
	f.Write(t, "file.txt", []byte("unchecked\n"))
	next := f.CommitFile(t, "unchecked")
	delivered := 0
	err := c.Receive(t.Context(), gitcache.ReceiveRequest{
		Repository: gittest.Repository, Actor: "agent", Fingerprint: "key",
		AlwaysRun: true, Admission: gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: f.Commit},
		Input: receiveInput(t, f, f.Commit, next), Output: io.Discard,
		Deliver: func(context.Context, gitcache.ReceiveEvent) ([]string, error) { delivered++; return nil, nil }})
	if err == nil || delivered != 0 {
		t.Fatal("unchecked receive acknowledged", err, delivered)
	}
	if got := f.Git(t, f.Store(), "rev-parse", "refs/heads/main"); got != f.Commit {
		t.Fatal("unchecked receive moved main", got)
	}
	return err
}
func TestNewRejectsUnusableConfiguredHook(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"missing", "not-executable", "empty", "directory"} {
		t.Run(kind, func(t *testing.T) { checkUnusableHook(t, kind) })
	}
}
func checkUnusableHook(t *testing.T, kind string) {
	t.Helper()

	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	hook := filepath.Join(f.Root, "bad-hook")
	switch kind {
	case "not-executable":
		if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 0\n"), 0600); err != nil {
			t.Fatal(err)
		}
	case "empty":
		if err := os.WriteFile(hook, nil, 0700); err != nil {
			t.Fatal(err)
		}
	case "directory":
		if err := os.Mkdir(hook, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cfg.HookBinary = hook
	c, err := gitcache.New(t.Context(), cfg)
	if c != nil {
		_ = c.Close()
	}
	if err == nil {
		t.Fatal("unusable hook accepted at startup", kind)
	}
}

func TestReceiveRefusesWhenHookDirectoryCannotBeInstalled(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	c := thirdInit(t, cfg)
	hooks := filepath.Join(f.Store(), "hooks")
	if err := os.Rename(hooks, hooks+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooks, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	err := assertUncheckedRefused(t, c, f)
	var unavailable *gitport.UnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatal("uninstalled hook reached native receive", err)
	}
	if _, err := c.PendingReceive(t.Context(), gittest.Repository); !os.IsNotExist(err) {
		t.Fatal("failed hook install left a receive", err)
	}
}
