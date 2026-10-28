package gitcache

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeHooksCreateMissingDirectoryAndReplaceScripts(t *testing.T) {
	repository := t.TempDir()
	if err := installReceiveHooks(repository); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pre-receive", "update"} {
		path := filepath.Join(repository, "hooks", name)
		if got, err := os.Readlink(path); err != nil || got != executable {
			t.Fatalf("hook=%q %v", got, err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := installReceiveHooks(repository); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pre-receive", "update"} {
		if got, err := os.Readlink(filepath.Join(repository, "hooks", name)); err != nil || got != executable {
			t.Fatalf("legacy hook retained %q %v", got, err)
		}
	}
	other := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(other, "hooks")); err != nil {
		t.Fatal(err)
	}
	if err := installReceiveHooks(other); err == nil {
		t.Fatal("symlink hooks directory accepted")
	}
}
func TestNativeHookDispatchRefusesMalformedInvocation(t *testing.T) {
	for _, args := range [][]string{{"/repo/hooks/pre-receive", "extra"}, {"/repo/hooks/update"}, {"/repo/hooks/update", "refs/heads/main", "bad", "bad"}} {
		if code, handled := ReceiveHookInvocation(context.Background(), args, strings.NewReader(""), io.Discard); !handled || code == 0 {
			t.Fatalf("accepted %q", args)
		}
	}
	if _, handled := ReceiveHookInvocation(context.Background(), []string{"oberth"}, strings.NewReader(""), io.Discard); handled {
		t.Fatal("normal server invocation handled as hook")
	}
}
