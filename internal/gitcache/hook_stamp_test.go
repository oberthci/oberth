package gitcache

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestHookIdentityDetectsSameSizeEditWithRestoredMtime(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "hook")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	h := &trustedHook{}
	if err := h.check(path); err != nil {
		t.Fatal(err)
	}
	stamp := h.info.ModTime()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := h.check(path); err == nil {
		t.Fatal("same-size hook edit with restored mtime passed identity check")
	}
}

func BenchmarkReceiveHookIdentity(b *testing.B) {
	path := filepath.Join(b.TempDir(), "hook")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0700)
	if err != nil {
		b.Fatal(err)
	}
	if _, err = io.CopyN(f, zeroReader{}, 123<<20); err != nil {
		b.Fatal(err)
	}
	if err = f.Close(); err != nil {
		b.Fatal(err)
	}
	c := &Cache{config: Config{HookBinary: path}}
	if err = c.loadHookIdentity(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := c.checkHook(); err != nil {
			b.Fatal(err)
		}
	}
}
