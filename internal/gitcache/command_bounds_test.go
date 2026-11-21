package gitcache

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These are command-output bounds, independently of supported source sizes.
func TestNativeCommandCaptureAcceptsNRefusesNPlusOne(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	c, err := New(t.Context(), Config{Root: t.TempDir(), Scratch: t.TempDir(), Binary: "/usr/bin/git"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	repo := "origin/team/widget"
	if err = c.Init(t.Context(), repo); err != nil {
		t.Fatal(err)
	}
	p, err := c.path(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int64{1 << 20, 2 << 20, 16 << 20, 32 << 20} {
		for _, delta := range []int64{0, 1} {
			cmd := exec.CommandContext(t.Context(), c.config.Binary, "hash-object", "-w", "--stdin")
			cmd.Dir = p
			cmd.Stdin = io.LimitReader(zeroReader{}, limit+delta)
			oid, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			data, err := c.capture(
				t.Context(),
				command{
					dir:  p,
					args: []string{"cat-file", "blob", strings.TrimSpace(string(oid))},
				},
				limit,
			)
			if (err == nil) != (delta == 0) {
				t.Fatalf("capture %d+%d: %v", limit, delta, err)
			}
			if err == nil && int64(len(data)) != limit {
				t.Fatalf("truncated successful capture %d/%d", len(data), limit)
			}
		}
	}
}

func TestStartupCleanupFailureRefusesOpenAndReleasesOwnership(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	root := t.TempDir()
	scratch := t.TempDir()
	owned := filepath.Join(scratch, "oberth-export-failed")
	if err := os.Mkdir(owned, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owned, "body"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(owned, 0500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(owned, 0700) }()
	cfg := Config{Root: root, Scratch: scratch, Binary: "/usr/bin/git"}
	c, err := New(t.Context(), cfg)
	if err == nil {
		_ = c.Close()
		t.Fatal("startup ignored failed unlink")
	}
	if err = os.Chmod(owned, 0700); err != nil {
		t.Fatal(err)
	}
	c, err = New(t.Context(), cfg)
	if err != nil {
		t.Fatal("failed open leaked root ownership", err)
	}
	defer func() { _ = c.Close() }()
	if _, err = os.Stat(owned); !os.IsNotExist(err) {
		t.Fatal("retry left partial export", err)
	}
}

func TestRepositoryGatesDoNotBlockOtherRepositories(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	c, err := New(t.Context(), Config{Root: t.TempDir(), Scratch: t.TempDir(), Binary: "/usr/bin/git"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	p, err := c.path("origin/team/busy")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.lock(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	defer c.unlock(p)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err = c.Init(ctx, "origin/team/other"); err != nil {
		t.Fatal(err)
	}
}

func TestNativeErrorDetailDrainsPastIts8192BytePrefix(t *testing.T) {
	t.Parallel()
	detail := &nativeDetail{}
	for _, n := range []int{8192, 1} {
		if got, err := detail.Write([]byte(strings.Repeat("x", n))); err != nil || got != n {
			t.Fatal("bounded detail stalled Git", got, err)
		}
		if detail.Len() != 8192 {
			t.Fatal("diagnostic prefix exceeded its cap", detail.Len())
		}
	}
}
